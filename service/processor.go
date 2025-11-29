package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"StoryToVideo-server/config"
	"StoryToVideo-server/models"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// Processor 处理队列任务
type Processor struct {
	DB             *gorm.DB
	WorkerEndpoint string
}

func NewProcessor(db *gorm.DB) *Processor {
	// 从配置中获取 Worker 地址
	return &Processor{
		DB:             db,
		WorkerEndpoint: config.AppConfig.Worker.Addr,
	}
}

// StartProcessor 启动任务消费者
func (p *Processor) StartProcessor(concurrency int) {
	srv := asynq.NewServer(
		asynq.RedisClientOpt{
			Addr:     config.AppConfig.Redis.Addr,
			Password: config.AppConfig.Redis.Password,
		},
		asynq.Config{
			Concurrency: concurrency,
			Queues: map[string]int{
				"default": 1,
			},
		},
	)
	mux := asynq.NewServeMux()
	mux.HandleFunc(TypeGenerateTask, p.HandleGenerateTask)

	log.Printf("Starting Task Processor with concurrency %d...", concurrency)
	go func() {
		if err := srv.Run(mux); err != nil {
			log.Fatalf("could not run server: %v", err)
		}
	}()
}

// HandleGenerateTask 核心处理逻辑
func (p *Processor) HandleGenerateTask(ctx context.Context, t *asynq.Task) error {
	var payload TaskPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("json.Unmarshal failed: %v: %w", err, asynq.SkipRetry)
	}

	// 1. 获取任务 (使用 GORM)
	task, err := models.GetTaskByIDGorm(p.DB, payload.TaskID)
	if err != nil {
		return fmt.Errorf("task not found: %v", err)
	}

	log.Printf("Processing Task: %s | Type: %s", task.ID, task.Type)
	task.UpdateStatus(p.DB, models.TaskStatusProcessing, nil, "")

	// 2. 构造 Worker 请求参数
	workerReq := map[string]interface{}{
		"task_id": task.ID,
		"type":    task.Type,
		"params":  task.Parameters,
	}

	// 提取 Prompt (兼容不同任务类型)
	if task.Type == models.TaskTypeShotGen {
		if val, ok := task.Parameters["prompt"]; ok {
			workerReq["prompt"] = val
		}
	} else {
		// 分镜生成任务通常从 story_text 提取
		if val, ok := task.Parameters["story_text"]; ok {
			workerReq["prompt"] = val
		}
	}

	// 3. 调用 Python Worker
	workerResp, err := p.callWorker(workerReq)
	if err != nil {
		log.Printf("Worker call failed: %v", err)
		task.UpdateStatus(p.DB, models.TaskStatusFailed, nil, fmt.Sprintf("Worker connection error: %v", err))
		return err // 返回 err 会触发 asynq 重试
	}

	// 4. 处理 Worker 业务错误
	if workerResp.Status != "success" {
		task.UpdateStatus(p.DB, models.TaskStatusFailed, nil, workerResp.Error)
		return nil // 业务失败，不再重试
	}

	// 5. 根据任务类型处理结果
	var processingErr error
	if task.Type == models.TaskTypeShotGen {
		// 单分镜生成/重绘
		processingErr = p.handleShotUpdate(task.ShotID, workerResp.Result)
	} else if task.Type == models.TaskTypeStoryboard {
		// 故事转分镜
		if err := p.handleStoryboardCreation(task.ID, task.ProjectID, workerResp.Result); err != nil {
			processingErr = fmt.Errorf("failed to create shots: %v", err)
		}
	}

	if processingErr != nil {
		log.Printf("[Error] Processing result failed: %v", processingErr)
		task.UpdateStatus(p.DB, models.TaskStatusFailed, workerResp.Result, processingErr.Error())
		return nil
	}

	// 6. 成功完成
	task.UpdateStatus(p.DB, models.TaskStatusSuccess, workerResp.Result, "")
	log.Printf("Task %s completed successfully", task.ID)
	return nil
}

// 辅助函数：处理分镜创建逻辑
func (p *Processor) handleStoryboardCreation(taskID string, projectID string, result map[string]interface{}) error {
	shotsData, ok := result["shots"]
	if !ok {
		return fmt.Errorf("worker response missing 'shots'")
	}
	shotsList, ok := shotsData.([]interface{})
	if !ok {
		return fmt.Errorf("'shots' is not a list")
	}

	// 临时结构体用于解析 Worker 返回的单个分镜
	type workerShot struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
	}

	var shotsToCreate []models.Shot
	for i, shotData := range shotsList {
		jsonBytes, _ := json.Marshal(shotData)
		var ws workerShot
		if err := json.Unmarshal(jsonBytes, &ws); err != nil {
			continue
		}

		newShot := models.Shot{
			ID:        uuid.NewString(),
			ProjectID: projectID,
			Title:     ws.Title,
			Description: ws.Description,
			Prompt:    ws.Prompt,
			Order:     i + 1,
			Status:    "pending",
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		shotsToCreate = append(shotsToCreate, newShot)
	}

	if len(shotsToCreate) > 0 {
		if err := models.BatchCreateShots(p.DB, shotsToCreate); err != nil {
			return err
		}
		log.Printf("Successfully created %d shots for project %s", len(shotsToCreate), projectID)
	}
	return nil
}

// 辅助函数：处理单分镜更新逻辑
func (p *Processor) handleShotUpdate(shotID string, result map[string]interface{}) error {
	if shotID == "" {
		return nil
	}
	shot, err := models.GetShotByIDGorm(p.DB, shotID)
	if err != nil {
		return fmt.Errorf("shot not found: %v", err)
	}

	// 解析图片 URL
	if images, ok := result["images"].([]interface{}); ok && len(images) > 0 {
		if url, ok := images[0].(string); ok {
			return shot.UpdateImage(p.DB, url)
		}
	}
	return nil
}

// Worker 响应结构
type WorkerResponse struct {
    Status string                 `json:"status"`
    Result map[string]interface{} `json:"result"` 
    Error  string                 `json:"error"`
}

func (p *Processor) callWorker(reqBody map[string]interface{}) (*WorkerResponse, error) {
	jsonBody, _ := json.Marshal(reqBody)
	resp, err := http.Post(p.WorkerEndpoint, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker status code: %d", resp.StatusCode)
	}

	var workerResp WorkerResponse
	if err := json.NewDecoder(resp.Body).Decode(&workerResp); err != nil {
		return nil, fmt.Errorf("decode failed: %v", err)
	}
	return &workerResp, nil
}