// ...existing code...
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

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
	// 标记为 processing
	if err := task.UpdateStatus(p.DB, models.TaskStatusProcessing, nil, ""); err != nil {
		log.Printf("UpdateStatus processing failed: %v", err)
	}

	// 2. 构造 Worker 请求参数
	workerReq := map[string]interface{}{
		"task_id": task.ID,
		"type":    task.Type,
		"params":  task.Parameters,
	}

	// 提取 prompt / 兼容不同任务类型
	switch task.Type {
	case models.TaskTypeShotImage:
		// 优先使用 task.Parameters.Sh ot.Prompt；若为空尝试从 shot 表读取
		if task.Parameters.Shot.Prompt != "" {
			workerReq["prompt"] = task.Parameters.Shot.Prompt
		} else if task.Parameters.Shot.ShotId != "" {
			if shot, err := models.GetShotByID(task.ProjectId, task.Parameters.Shot.ShotId); err == nil {
				workerReq["prompt"] = shot.Prompt
			}
		}
	case models.TaskTypeProjectText:
		// 可把 project.story_text 或 shot_defaults.storyText 发给 Worker 作为上下文
		if task.Parameters.ShotDefaults.StoryText != "" {
			workerReq["prompt"] = task.Parameters.ShotDefaults.StoryText
		} else {
			var project models.Project
			if err := p.DB.First(&project, "id = ?", task.ProjectId).Error; err == nil {
				workerReq["prompt"] = project.StoryText
			}
		}
	case models.TaskTypeProjectVideo, models.TaskTypeProjectAudio:
		// 这些任务通常不需要单条 prompt，这里可以把 project id 作为 context 传入
		workerReq["project_id"] = task.ProjectId
	}

	// 3. 调用 Worker
	workerResp, err := p.callWorker(workerReq)
	if err != nil {
		log.Printf("Worker call failed: %v", err)
		_ = task.UpdateStatus(p.DB, models.TaskStatusFailed, nil, fmt.Sprintf("Worker connection error: %v", err))
		return err // 返回 err 会触发 asynq 重试
	}

	// 4. 处理 Worker 业务错误
	if workerResp.Status != "success" {
		_ = task.UpdateStatus(p.DB, models.TaskStatusFailed, nil, workerResp.Error)
		return nil // 业务失败，不再重试
	}

	// 5. 根据任务类型处理结果
	var processingErr error

	switch task.Type {
	case models.TaskTypeProjectText:
		// 创建 shots（若 Worker 返回了 shots 列表），并解锁依赖该文本任务的 shot_image 任务
		createdShots, err := p.handleStoryboardCreation(task.ID, task.ProjectId, workerResp.Result)
		if err != nil {
			processingErr = fmt.Errorf("handleStoryboardCreation failed: %v", err)
		} else {
			// 解锁依赖此文本任务的被阻塞任务（将 parameters 填充 shotId/prompt 并设为 pending，然后入队）
			if err := p.unlockDependentShotTasks(task.ID, task.ProjectId, createdShots); err != nil {
				log.Printf("unlockDependentShotTasks failed: %v", err)
			}
		}
	case models.TaskTypeShotImage:
		// 使用 parameters.shot.shotId 更新对应 shot 的图片路径
		processingErr = p.handleShotUpdate(task.Parameters.Shot.ShotId, workerResp.Result)
	case models.TaskTypeProjectVideo:
		// project_video：workerResp.Result 里可能包含视频地址 / meta，直接写入 task.result 由客户端读取
	case models.TaskTypeProjectAudio:
		// project_audio：同上
	default:
		// 未知类型：仅记录 result
	}

	if processingErr != nil {
		log.Printf("[Error] Processing result failed: %v", processingErr)
		_ = task.UpdateStatus(p.DB, models.TaskStatusFailed, workerResp.Result, processingErr.Error())
		return nil
	}

	// 6. 成功完成：把 Worker 返回的 result 写入 task.result，并标记 finished
	if err := task.UpdateStatus(p.DB, models.TaskStatusSuccess, workerResp.Result, ""); err != nil {
		log.Printf("UpdateStatus finished failed: %v", err)
	}
	log.Printf("Task %s completed successfully", task.ID)
	return nil
}

// handleStoryboardCreation：解析 worker 返回并批量创建 shots，返回创建的 shots 列表
func (p *Processor) handleStoryboardCreation(_ string, projectID string, result map[string]interface{}) ([]models.Shot, error) {
	shotsData, ok := result["shots"]
	if !ok {
		return nil, fmt.Errorf("worker response missing 'shots'")
	}
	shotsListIface, ok := shotsData.([]interface{})
	if !ok {
		return nil, fmt.Errorf("'shots' is not a list")
	}

	// 临时结构体用于解析 Worker 返回的单个分镜
	type workerShot struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
	}

	var shotsToCreate []models.Shot
	for i, shotData := range shotsListIface {
		jsonBytes, _ := json.Marshal(shotData)
		var ws workerShot
		if err := json.Unmarshal(jsonBytes, &ws); err != nil {
			continue
		}

		newShot := models.Shot{
			ID:          uuid.NewString(),
			ProjectId:   projectID,
			Title:       ws.Title,
			Description: ws.Description,
			Prompt:      ws.Prompt,
			Order:       i + 1,
			Status:      models.ShotStatusPending,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		shotsToCreate = append(shotsToCreate, newShot)
	}

	if len(shotsToCreate) > 0 {
		if err := models.BatchCreateShots(p.DB, shotsToCreate); err != nil {
			return nil, err
		}
		log.Printf("Successfully created %d shots for project %s", len(shotsToCreate), projectID)
	}

	// 从 DB 读取刚创建的 shots（按 order 排序）
	shots, err := models.GetShotsByProjectID(projectID)
	if err != nil {
		return nil, err
	}

	return shots, nil
}

// unlockDependentShotTasks：把依赖 textTaskID 的 blocked shot tasks 解锁（填充 shotId/prompt 并设为 pending，入队）
func (p *Processor) unlockDependentShotTasks(textTaskID string, _ string, createdShots []models.Shot) error {
	// 从原生 SQL 查出被阻塞的任务 id（parameters JSON 的 depends_on = textTaskID）
	rows, err := models.DB.Query(`SELECT id FROM task WHERE status = ? AND JSON_EXTRACT(parameters, '$.depends_on') = ?`, models.TaskStatusBlocked, textTaskID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		taskIDs = append(taskIDs, id)
	}

	if len(taskIDs) == 0 {
		return nil
	}

	// 将 createdShots 按 order 排序（GetShotsByProjectID 已保证），并为每个被阻塞任务分配一个 shot（按顺序）
	for i, tid := range taskIDs {
		if i >= len(createdShots) {
			// 如果任务比生成的 shot 多，则跳出或只解锁已有的
			break
		}
		shot := createdShots[i]

		// 读取任务以便修改 parameters（使用 SQL 版本）
		t, err := models.GetTaskByID(tid)
		if err != nil {
			log.Printf("GetTaskByID failed for %s: %v", tid, err)
			continue
		}

		// 填充 parameters.shot.shotId / prompt
		t.Parameters.Shot.ShotId = shot.ID
		// 若 shot 提示为空，则保持原有 prompt
		if t.Parameters.Shot.Prompt == "" {
			t.Parameters.Shot.Prompt = shot.Prompt
		}

		// marshal parameters 并更新任务的 parameters + status
		paramsBytes, _ := json.Marshal(t.Parameters)
		if _, err := models.DB.Exec(`UPDATE task SET parameters = ?, status = ?, updated_at = ? WHERE id = ?`, paramsBytes, models.TaskStatusPending, time.Now(), tid); err != nil {
			log.Printf("failed to update task parameters/status for %s: %v", tid, err)
			continue
		}
		// 将任务入队
		if err := EnqueueTask(tid); err != nil {
			log.Printf("EnqueueTask failed for %s: %v", tid, err)
			// 不回滚状态，但记录日志
		} else {
			log.Printf("Unblocked and enqueued task %s (shot %s)", tid, shot.ID)
		}
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
