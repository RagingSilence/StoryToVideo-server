// ...existing code...
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
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
	resultData, err := p.pollJobResult(jobID)
	if err != nil {
		log.Printf("轮询任务失败: %v", err)
		task.UpdateStatus(p.DB, models.TaskStatusFailed, nil, fmt.Sprintf("Job Failed: %v", err))
		return nil // 业务失败，不再重试
	}

// 3. 根据类型处理结果 (TOS存储 + DB更新)

	var processingErr error

	switch task.Type {

	case models.TaskTypeStoryboard: // 故事 -> 分镜
		processingErr = p.handleStoryboardResult(task.ProjectId, resultData)
	case models.TaskTypeShotGen,"regenerate_shot": //关键帧 -> 生图,or 重新生成图像
		shotId := task.ShotId
		//先使用任务中的 ShotId，如果没有则尝试从参数中获取
        if  shotId == "" && task.Parameters.Shot != nil {
			shotId = task.Parameters.Shot.ShotId 
		}
		processingErr = p.handleImageResult(shotId, resultData)

	case models.TaskTypeTTS:        // 文本 -> 语音
		shotId := task.ShotId
        if  shotId == "" && task.Parameters.Shot != nil { 
			shotId = task.Parameters.Shot.ShotId 
		}
		processingErr = p.handleTTSResult(shotId, resultData)

	case models.TaskTypeVideoGen: // 图 -> 视频
		shotId := task.ShotId
		if shotId == "" && task.Parameters.Shot != nil { 
        shotId = task.Parameters.Shot.ShotId 
    }
    processingErr = p.handleVideoResult(shotId, resultData)

	default:
		processingErr = fmt.Errorf("unknown task type: %s", task.Type)
	}

	if processingErr != nil {
		log.Printf("[Error] 数据处理失败: %v", processingErr)
		task.UpdateStatus(p.DB, models.TaskStatusFailed, resultData, processingErr.Error())
		return nil
	}

	// 5. 成功完成
	task.UpdateStatus(p.DB, models.TaskStatusSuccess, resultData, "")
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
	case models.TaskTypeStoryboard:// 故事 -> 分镜
		// 获取项目故事文本
		apiPath = "/v1/llm/storyboard"
		var project models.Project
		if err := p.DB.First(&project, "id = ?", task.ProjectId).Error; err != nil {
			return "", fmt.Errorf("project not found: %v", err)
		}
		params := task.Parameters.ShotDefaults
        if params == nil { return "", fmt.Errorf("missing shot_defaults parameters") }

		targetShots := params.ShotCount
		if targetShots <= 0 {
			targetShots = 8 
		}
		extras := map[string]interface{}{
			"tone":              "warm", //
			"duration_hint_sec": project.Duration,
		}
		if extras["duration_hint_sec"] == 0 {
			extras["duration_hint_sec"] = 5 // 默认 5s
		}
		reqBody = map[string]interface{}{
			"story_id":     project.ID,        // 对应 story_id
			"story_text":   params.StoryText, // 对应 story_text
			"style":        params.Style,     // 对应 style
			"target_shots": targetShots,       // 对应 target_shots
			"lang":         "zh",              // 对应 lang
			"extras":       extras,            // 对应 extras 对象
			//"task_id":         task.ID,
		}
		
	case models.TaskTypeShotGen, "regenerate_shot":
		// 获取分镜 Prompt
		apiPath = "/v1/image/generate"
		params := task.Parameters.Shot
		if params == nil { return "", fmt.Errorf("missing shot parameters") }

		width, _ := strconv.Atoi(params.ImageWidth)
		if width == 0 {
			width = 1024
		}
		height, _ := strconv.Atoi(params.ImageHeight)
		if height == 0 {
			height = 576
		}
		sizeStr := fmt.Sprintf("%dx%d", width, height)

		// 构造请求体
		reqBody = map[string]interface{}{
			"shot_id":         params.ShotId,                 // 对应 shot_id
			"prompt":          params.Prompt,// 对应 prompt (使用任务中的提示词)
			"negative_prompt": "blurry, lowres, bad anatomy, text, watermark", // 默认负向提示词
			"style":           params.Style,  // 对应 style
			"size":            sizeStr,                     // 对应 size
			"model":           "sd-turbo",                  // 默认模型，可改为从 Parameters 读取
			"seed":            42,                          // 默认种子，建议改为 -1 或随机
			"steps":           20,                          // 默认步数
			"guidance_scale":  7.5,                         // 默认引导系数
			"scheduler":       "euler_a",                   // 默认调度器
			"safety_check":    false,                       // 是否开启安全检查
			//"task_id":         task.ID,                     // 额外携带 TaskID
		}

	case models.TaskTypeTTS:
		// 获取分镜文本
		apiPath = "/v1/audio/generate"
		params := task.Parameters.TTS
		if params == nil { return "", fmt.Errorf("missing tts parameters") }
		text := params.Text
        if text == "" && task.ShotId != "" {
            shot, _ := models.GetShotByIDGorm(p.DB, task.ShotId)
            if shot != nil {
                text = shot.Description // 使用分镜描述作为文本
            }
        }
        reqBody = map[string]interface{}{
            "text":        text,
            "voice":    params.Voice,
            "language":    params.Lang,
            "sample_rate": params.SampleRate,
            "format":      params.Format,
        }

	case models.TaskTypeVideoGen: // 图 -> 视频
		// 获取分镜图像路径
		apiPath = "/v1/video/generate"
		parameters := task.Parameters.Video
		if parameters == nil { return "", fmt.Errorf("missing video parameters") }

		shot, err := models.GetShotByIDGorm(p.DB, task.ShotId)
		if err != nil {
			return "", fmt.Errorf("shot not found")
		}
		if shot.ImagePath == "" {
			return "", fmt.Errorf("shot has no image_path (unable to gen video)")
		}
		// 1. 处理数值转换 (FPS)
		fps := 24 // 默认值
		if parameters.FPS != 0 {
			fps = parameters.FPS
		}
		
		// 2. 处理分辨率
		resolution := "1280x720" // 默认值
		if parameters.Resolution != "" {
			resolution = parameters.Resolution
		}
		//音频不知道怎么做，先不传
		reqBody = map[string]interface{}{
			"shot_id":         shot.ID,
			"image_url":       shot.ImagePath, 
			"duration_sec":    5,              
			"fps":             fps,
			"resolution":      resolution,
			"model":           "svd-img2vid",
			"transition":      shot.Transition,
			"motion_strength": 0.7,
			"seed":            time.Now().Unix() % 100000, 
			//"audio":           audioObj,
			// "task_id": task.ID, // 如果对方接口允许额外字段则加上，否则去掉
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

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("worker status code: %d", resp.StatusCode)
	}

	// 解析响应，获取 Job ID
	var respData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return "", fmt.Errorf("decode response failed: %v", err)
	}

	// 假设接口返回格式为: {"job_id": "xxx", "status": "pending"} 
	// 或者直接返回 {"id": "xxx"}
	if jobID, ok := respData["job_id"].(string); ok {
    return jobID, nil
	}
	if jobID, ok := respData["id"].(string); ok {
		return jobID, nil
	}
	return "", fmt.Errorf("response missing 'job_id' or 'id': %v", respData)
}

// pollJobResult 轮询 GET /v1/jobs/{job_id} 直到完成
func (p *Processor) pollJobResult(jobID string) (map[string]interface{}, error) {
	jobURL := fmt.Sprintf("%s/v1/jobs/%s", p.WorkerEndpoint, jobID)
	
	// 修改这里：直接使用硬编码或默认值，因为 config 中没有定义 timeout_minutes
	timeoutDuration := 30 * time.Minute 
	timeout := time.After(timeoutDuration)
	ticker := time.NewTicker(3 * time.Second) // 每 3 秒轮询一次
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("polling timeout")
		case <-ticker.C:
			// 发起查询
			resp, err := http.Get(jobURL)
			if err != nil {
				log.Printf("轮询网络错误(重试中): %v", err)
				continue
			}
			
			var jobData map[string]interface{}
            err = json.NewDecoder(resp.Body).Decode(&jobData)
            resp.Body.Close() 
            
            if err != nil {
                continue
            }

			// 检查状态 (字段为 status: pending/processing/success/failed)
			status, _ := jobData["status"].(string)
			log.Printf("Job %s Status: %s", jobID, status)

			if status == "success" || status == "finished" || status == "completed" {
				// 获取结果数据
				if result, ok := jobData["result"].(map[string]interface{}); ok {
					return result, nil
				}
				return jobData, nil // 容错
			}

			if status == "failed" || status == "error" {
				errMsg, _ := jobData["error"].(string)
				return nil, fmt.Errorf("worker reported failure: %s", errMsg)
			}
			// 继续等待 (pending/processing)
		}
	}
}

func (p *Processor) handleStoryboardResult(projectID string, result map[string]interface{}) error {
	shotsData, ok := result["shots"] // 假设返回 {"shots": [...]}
	if !ok {
		return fmt.Errorf("missing 'shots' in result")
	}
	shotsListIface, ok := shotsData.([]interface{})
	if !ok {
		return nil, fmt.Errorf("'shots' is not a list")
	}

	var shotsToCreate []models.Shot
	for i, item := range shotsList {
		shotMap, _ := item.(map[string]interface{})
		
		newShot := models.Shot{
			ID:          uuid.NewString(),
			ProjectId:   projectID,
			Title:       getString(shotMap, "title"),
			Description: getString(shotMap, "description"), // 旁白/描述
			Prompt:      getString(shotMap, "prompt"),      // 画面提示词
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

// 处理图像生成结果 -> 更新 ImagePath
func (p *Processor) handleImageResult(shotID string, result map[string]interface{}) error {
	// 假设返回 {"image_url": "http://tos..."} 或 {"images": ["..."]}
	var imageURL string
	
	if url, ok := result["image_url"].(string); ok {
		imageURL = url
	} else if images, ok := result["images"].([]interface{}); ok && len(images) > 0 {
		if url, ok := images[0].(string); ok {
			imageURL = url
		}
	}

	if imageURL == "" {
		return fmt.Errorf("no image url found in result")
	}
	
	// 下载并上传到 MinIO
	finalURL, err := downloadAndUploadToMinIO(imageURL, fmt.Sprintf("shots/%s/image.png", shotID))
    if err != nil {
        log.Printf("上传到 MinIO 失败，使用原始 URL: %v", err)
        finalURL = imageURL // 容错：使用原始 URL
    }
	
	shot, err := models.GetShotByIDGorm(p.DB, shotID)
	if err != nil {
		return err
	}
	// 更新数据库
	// 修改这里：使用 finalURL 而不是 imageURL
	return shot.UpdateImage(p.DB, finalURL)
}

func (p *Processor) handleTTSResult(shotId string, result map[string]interface{}) error {
    // 假设 Worker 返回 {"audio_url": "...", "base64": "..."}
    audioUrl, ok := result["audio_url"].(string) // 或者可能是临时链接
    if !ok { return fmt.Errorf("no audio_url in result") }

    // 1. 转存到自己的 MinIO/TOS
    finalUrl, err := downloadAndUploadToMinIO(audioUrl, fmt.Sprintf("shots/%s/audio.mp3", shotId))
    if err != nil {
        return err
    }

    // 2. 更新数据库
    return p.DB.Model(&models.Shot{}).Where("id = ?", shotId).Updates(map[string]interface{}{
        "audio_path": finalUrl,
        "updated_at": time.Now(),
    }).Error
}
// 处理视频生成结果 -> 更新 VideoUrl
func (p *Processor) handleVideoResult(shotID string, result map[string]interface{}) error {
	// 假设返回 {"video_url": "http://tos..."}
	videoURL, ok := result["video_url"].(string)
	if !ok || videoURL == "" {
		return fmt.Errorf("missing 'video_url' in result")
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

// 工具函数：安全获取 string
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
