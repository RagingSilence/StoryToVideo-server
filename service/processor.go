// ...existing code...
package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
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

	if task.Type == "create_project" {
		// 直接标记为完成
		task.UpdateStatus(p.DB, models.TaskStatusSuccess, nil, "Project initialized")
		return nil
		}
	jobID, err := p.dispatchWorkerRequest(task)
	if err != nil {
		log.Printf("Worker 请求失败: %v", err)
		task.UpdateStatus(p.DB, models.TaskStatusFailed, nil, fmt.Sprintf("Worker Request Failed: %v", err))
		return err // 返回 err 触发重试
	}
	log.Printf("任务已提交，Job ID: %s，开始轮询结果...", jobID)
	taskResult, err := p.pollJobResult(jobID)
	if err != nil {
		log.Printf("轮询任务失败: %v", err)
		task.UpdateStatus(p.DB, models.TaskStatusFailed, nil, fmt.Sprintf("Job Failed: %v", err))
		return nil // 业务失败，不再重试
	}

	// 3. 根据类型处理结果 (TOS存储 + DB更新)
	var processingErr error

	switch task.Type {
	case models.TaskTypeStoryboard: // 故事 -> 分镜
		processingErr = p.handleStoryboardResult(task.ProjectId, taskResult)

	case models.TaskTypeShotImage, "regenerate_shot": // 关键帧 -> 生图, or 重新生成图像
		shotId := task.ShotId
		if shotId == "" && task.Parameters.Shot != nil {
			shotId = task.Parameters.Shot.ShotId
		}
		processingErr = p.handleImageResult(shotId, taskResult)

	case models.TaskTypeProjectAudio: // 文本 -> 语音
		shotId := task.ShotId
		if shotId == "" && task.Parameters.Shot != nil {
			shotId = task.Parameters.Shot.ShotId
		}
		processingErr = p.handleTTSResult(shotId, taskResult)

	case models.TaskTypeVideoGen: // 图 -> 视频
		shotId := task.ShotId
		if shotId == "" && task.Parameters.Shot != nil {
			shotId = task.Parameters.Shot.ShotId
		}
		processingErr = p.handleVideoResult(shotId, taskResult)

	default:
		processingErr = fmt.Errorf("unknown task type: %s", task.Type)
	}

	if processingErr != nil {
		log.Printf("[Error] 数据处理失败: %v", processingErr)
		task.UpdateStatus(p.DB, models.TaskStatusFailed, taskResult, processingErr.Error())
		return nil
	}

	// 5. 成功完成
	task.UpdateStatus(p.DB, models.TaskStatusSuccess, taskResult, "")
	log.Printf("Task %s completed successfully", task.ID)
	return nil
}
// ============================================================================
// 通信层：请求分发与轮询
// ============================================================================

// dispatchWorkerRequest 发送 POST 请求，返回 job_id
func (p *Processor) dispatchWorkerRequest(task *models.Task) (string, error) {
	var apiPath string
	var reqBody map[string]interface{}

	switch task.Type {
	case models.TaskTypeStoryboard: // 故事 -> 分镜
		apiPath = "/v1/llm/storyboard"
		var project models.Project
		if err := p.DB.First(&project, "id = ?", task.ProjectId).Error; err != nil {
			return "", fmt.Errorf("project not found: %v", err)
		}
		params := task.Parameters.ShotDefaults
		if params == nil {
			return "", fmt.Errorf("missing shot_defaults parameters")
		}

		// 构造请求体：基于现有数据结构
		reqBody = map[string]interface{}{
			"story_id":     project.ID,
			"story_text":   params.StoryText,
			"style":        params.Style,
			"target_shots": params.ShotCount,
			"lang":         "zh", // 默认中文，可扩展
		}

	case models.TaskTypeShotImage, "regenerate_shot":
		// 文生图：生成关键帧
		apiPath = "/v1/image/generate"
		params := task.Parameters.Shot
		if params == nil {
			return "", fmt.Errorf("missing shot parameters")
		}

		width, _ := strconv.Atoi(params.ImageWidth)
		if width == 0 { width = 1024 }
		height, _ := strconv.Atoi(params.ImageHeight)
		if height == 0 { height = 576 }
		sizeStr := fmt.Sprintf("%dx%d", width, height)

		reqBody = map[string]interface{}{
			"shot_id":         params.ShotId,
			"prompt":          params.Prompt,
			"negative_prompt": "blurry, lowres, bad anatomy, text, watermark",
			"style":           params.Style,
			"size":            sizeStr,
			"model":           "sd-turbo", // 默认模型
		}

	case models.TaskTypeProjectAudio:
		// TTS 生成
		apiPath = "/v1/audio/generate"
		params := task.Parameters.TTS
		if params == nil {
			return "", fmt.Errorf("missing tts parameters")
		}
		text := params.Text
		if text == "" && task.ShotId != "" {
			shot, _ := models.GetShotByIDGorm(p.DB, task.ShotId)
			if shot != nil {
				text = shot.Description
			}
		}
		reqBody = map[string]interface{}{
			"text":        text,
			"voice":       params.Voice,
			"language":    params.Lang,
			"sample_rate": params.SampleRate,
			"format":      params.Format,
		}

	case models.TaskTypeVideoGen: // 图 -> 视频
		apiPath = "/v1/video/generate"
		parameters := task.Parameters.Video
		if parameters == nil {
			return "", fmt.Errorf("missing video parameters")
		}

		shot, err := models.GetShotByIDGorm(p.DB, task.ShotId)
		if err != nil {
			return "", fmt.Errorf("shot not found")
		}
		if shot.ImagePath == "" {
			return "", fmt.Errorf("shot has no image_path (unable to gen video)")
		}

		fps := 24
		if parameters.FPS != 0 { fps = parameters.FPS }
		
		resolution := "1280x720"
		if parameters.Resolution != "" { resolution = parameters.Resolution }

		reqBody = map[string]interface{}{
			"shot_id":         shot.ID,
			"image_url":       shot.ImagePath, 
			"duration_sec":    4,
			"fps":             fps,
			"resolution":      resolution,
			"model":           "svd-img2vid",
			"transition":      shot.Transition,
			"motion_strength": 0.7,
		}

		// 如果分镜有音频，传给视频生成接口
		if shot.AudioPath != "" {
			reqBody["audio"] = map[string]interface{}{
				"voiceover_url": shot.AudioPath,
				"ducking": true,
			}
		}

	default:
		return "", fmt.Errorf("unsupported task type: %s", task.Type)
	}

	// 发送 HTTP 请求
	fullURL := p.WorkerEndpoint + apiPath
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request failed: %v", err)
	}
	log.Printf("POST %s", fullURL)
	
	resp, err := http.Post(fullURL, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("worker status code: %d", resp.StatusCode)
	}

	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return "", fmt.Errorf("decode response failed: %v", err)
	}

	// 优先返回根节点的 id
	if id, ok := respData["id"].(string); ok {
		return id, nil
	}
	if jobID, ok := respData["job_id"].(string); ok {
		return jobID, nil
	}
	return "", fmt.Errorf("response missing 'id'")
}

// pollJobResult 轮询 GET /v1/jobs/{job_id} 直到完成，返回 TaskResult
func (p *Processor) pollJobResult(jobID string) (*models.TaskResult, error) {
	jobURL := fmt.Sprintf("%s/v1/jobs/%s", p.WorkerEndpoint, jobID)
	
	timeoutDuration := 30 * time.Minute
	timeout := time.After(timeoutDuration)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("polling timeout")
		case <-ticker.C:
			resp, err := http.Get(jobURL)
			if err != nil {
				log.Printf("轮询网络错误(重试中): %v", err)
				continue
			}
			
			// 模型端返回完整的 Task 对象
			var taskResp models.Task
			if err := json.NewDecoder(resp.Body).Decode(&taskResp); err != nil {
				resp.Body.Close()
				log.Printf("解析响应失败: %v", err)
				continue
			}
			resp.Body.Close()

			status := taskResp.Status
			if status == models.TaskStatusSuccess || status == "success" || status == "completed" || status == "succeeded" {
				return &taskResp.Result, nil
			}

			if status == models.TaskStatusFailed || status == "error" {
				return nil, fmt.Errorf("worker reported failure: %s", taskResp.Error)
			}
			// 继续轮询
		}
	}
}

func (p *Processor) handleStoryboardResult(projectID string, result *models.TaskResult) error {
	// 从 TaskResult.Data 中获取 shots 数组
	shotsData, ok := result.Data["shots"]
	if !ok {
		return fmt.Errorf("missing 'shots' in result.Data")
	}
	shotsList, ok := shotsData.([]interface{})
	if !ok {
		return fmt.Errorf("'shots' is not a list")
	}

	var shotsToCreate []models.Shot
	for i, item := range shotsList {
		shotMap, _ := item.(map[string]interface{})

		newShot := models.Shot{
			ID:          uuid.NewString(),
			ProjectId:   projectID,
			Title:       getString(shotMap, "title"),
			Description: getString(shotMap, "description"),
			Prompt:      getString(shotMap, "prompt"),
			Order:       i + 1,
			Status:      models.ShotStatusPending,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		shotsToCreate = append(shotsToCreate, newShot)
	}

	if len(shotsToCreate) > 0 {
		if err := models.BatchCreateShots(p.DB, shotsToCreate); err != nil {
			return err
		}
	}
	log.Printf("Successfully created %d shots for project %s", len(shotsToCreate), projectID)
	return nil
}

// 处理图像生成结果 -> 更新 ImagePath
func (p *Processor) handleImageResult(shotID string, result *models.TaskResult) error {
	var finalURL string
	var err error
	objectName := fmt.Sprintf("shots/%s/image.png", shotID)

	// 优先使用 ResourceUrl
	if result.ResourceUrl != "" {
		finalURL, err = downloadAndUploadToMinIO(result.ResourceUrl, objectName)
		if err != nil {
			log.Printf("上传到 MinIO 失败，使用原始 URL: %v", err)
			finalURL = result.ResourceUrl
		}
	} else if result.Data != nil {
		// 从 Data 中获取图片数据 (Base64 或 URL)
		if imageData, ok := result.Data["image"].(string); ok && imageData != "" {
			if strings.HasPrefix(imageData, "http://") || strings.HasPrefix(imageData, "https://") {
				finalURL, err = downloadAndUploadToMinIO(imageData, objectName)
				if err != nil {
					log.Printf("上传到 MinIO 失败，使用原始 URL: %v", err)
					finalURL = imageData
				}
			} else {
				// Base64 格式
				finalURL, err = uploadBase64ToMinIO(imageData, objectName)
				if err != nil {
					return fmt.Errorf("上传 Base64 图片失败: %v", err)
				}
			}
		} else if imageURL, ok := result.Data["image_url"].(string); ok && imageURL != "" {
			finalURL, err = downloadAndUploadToMinIO(imageURL, objectName)
			if err != nil {
				log.Printf("上传到 MinIO 失败，使用原始 URL: %v", err)
				finalURL = imageURL
			}
		}
	}

	if finalURL == "" {
		log.Printf("[DEBUG] handleImageResult: ResourceUrl=%s, Data=%+v", result.ResourceUrl, result.Data)
		return fmt.Errorf("no image data found in result")
	}

	shot, err := models.GetShotByIDGorm(p.DB, shotID)
	if err != nil {
		return err
	}
	log.Printf("图片上传成功: %s", finalURL)
	return shot.UpdateImage(p.DB, finalURL)
}

func (p *Processor) handleTTSResult(shotId string, result *models.TaskResult) error {
	var audioURL string

	// 优先使用 ResourceUrl
	if result.ResourceUrl != "" {
		audioURL = result.ResourceUrl
	} else if result.Data != nil {
		if url, ok := result.Data["audio_url"].(string); ok {
			audioURL = url
		}
	}

	if audioURL == "" {
		return fmt.Errorf("no audio_url in result")
	}

	// 转存到自己的 MinIO/TOS
	finalURL, err := downloadAndUploadToMinIO(audioURL, fmt.Sprintf("shots/%s/audio.mp3", shotId))
	if err != nil {
		return err
	}

	// 更新数据库
	return p.DB.Model(&models.Shot{}).Where("id = ?", shotId).Updates(map[string]interface{}{
		"audio_path": finalURL,
		"updated_at": time.Now(),
	}).Error
}
// 处理视频生成结果 -> 更新 VideoUrl
func (p *Processor) handleVideoResult(shotID string, result *models.TaskResult) error {
	var videoURL string

	// 优先使用 ResourceUrl
	if result.ResourceUrl != "" {
		videoURL = result.ResourceUrl
	} else if result.Data != nil {
		if url, ok := result.Data["video_url"].(string); ok {
			videoURL = url
		}
	}

	if videoURL == "" {
		return fmt.Errorf("missing video_url in result")
	}

	// 下载并上传到 MinIO
	finalURL, err := downloadAndUploadToMinIO(videoURL, fmt.Sprintf("shots/%s/video.mp4", shotID))
	if err != nil {
		log.Printf("上传到 MinIO 失败，使用原始 URL: %v", err)
		finalURL = videoURL
	}

	return p.DB.Model(&models.Shot{}).Where("id = ?", shotID).Updates(map[string]interface{}{
		"video_url":  finalURL,
		"status":     models.ShotStatusCompleted,
		"updated_at": time.Now(),
	}).Error
}

func downloadAndUploadToMinIO(sourceURL, objectName string) (string, error) {
    // 1. 下载文件
    resp, err := http.Get(sourceURL)
    if err != nil {
        return "", fmt.Errorf("download failed: %v", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return "", fmt.Errorf("download status: %d", resp.StatusCode)
    }

    return UploadToMinIO(resp.Body, objectName, resp.ContentLength)
}

// uploadBase64ToMinIO 将 Base64 编码的图片解码后上传到 MinIO
func uploadBase64ToMinIO(base64Data, objectName string) (string, error) {
    // 移除可能的 data URI 前缀 (如 "data:image/png;base64,")
    if idx := strings.Index(base64Data, ","); idx != -1 {
        base64Data = base64Data[idx+1:]
    }
    
    // 解码 Base64
    imageData, err := base64.StdEncoding.DecodeString(base64Data)
    if err != nil {
        // 尝试 RawStdEncoding (无 padding)
        imageData, err = base64.RawStdEncoding.DecodeString(base64Data)
        if err != nil {
            return "", fmt.Errorf("base64 decode failed: %v", err)
        }
    }
    
    log.Printf("Base64 解码成功，图片大小: %d bytes", len(imageData))
    
    // 使用 bytes.Reader 创建 io.Reader
    reader := bytes.NewReader(imageData)
    
    return UploadToMinIO(reader, objectName, int64(len(imageData)))
}

// 工具函数：安全获取 string
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
