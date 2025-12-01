// ...existing code...
package models

import (
	"time"

	"gorm.io/gorm"
)

// 任务状态（在系统中统一使用这些状态）
const (
	// pending: 任务已就绪，等待执行器取走执行
	TaskStatusPending = "pending"
	// blocked: 任务因依赖未满足被阻塞（例如 shot_image 等待 project_text 完成）
	TaskStatusBlocked = "blocked"
	// processing: 任务正在执行中
	TaskStatusProcessing = "processing"
	// finished: 任务成功完成（保存 result 中的资源定位信息）
	TaskStatusSuccess = "finished"
	// failed: 任务执行失败（可以重试或人工干预）
	TaskStatusFailed = "failed"

	// 四类任务类型（严格区分）
	TaskTypeProjectText  = "project_text"  // 生成项目整体故事文本（包含每个 shot 的文字信息）
	TaskTypeShotImage    = "shot_image"    // 根据单个 shot 的文字信息生成图片
	TaskTypeProjectVideo = "project_video" // 根据项目全部 shot 生成整段视频
	TaskTypeProjectAudio = "project_audio" // 根据项目故事文本生成整段配音（整片音频）
)

type Task struct {
	ID        string `gorm:"primaryKey;type:varchar(64)" json:"id"`
	ProjectId string `json:"projectId"`
	// ShotId 可选：当任务与某个具体 shot 关联时填充（shot_image 类型通常会填）
	//ShotId            string         `json:"shotId,omitempty"`
	Type       string         `json:"type"`
	Status     string         `json:"status"`
	Progress   int            `json:"progress"`
	Message    string         `json:"message"`
	Parameters TaskParameters `gorm:"type:json" json:"parameters"`
	// Result 仅包含资源定位信息（客户端根据该信息去查询具体资源）
	Result            TaskResult `gorm:"type:json" json:"result"`
	Error             string     `json:"error"`
	EstimatedDuration int        `json:"estimatedDuration"`
	StartedAt         time.Time  `json:"startedAt"`
	FinishedAt        time.Time  `json:"finishedAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// 任务参数：根据任务类型选择使用对应子结构
type TaskParameters struct {
	// ProjectText 任务使用 ShotGenerationDefaults 来指定希望产生的 shot 数量、风格等
	ShotDefaults ShotGenerationParameters `json:"shot_defaults,omitempty"`
	// ShotImage 任务使用此结构（包含 prompt/尺寸/依赖信息）
	Shot TaskShotParameters `json:"shot,omitempty"`
	// ProjectVideo 任务的可选视频参数（覆盖 Project 的默认视频参数）
	Video VideoParameters `json:"video,omitempty"`
	// ProjectAudio 任务的可选 TTS 参数（覆盖 Project 的默认 tts 参数）
	TTS TaskTTSParameters `json:"tts,omitempty"`

	// 通用依赖：例如某些 shot_image 任务可以 depends_on 为 project_text 任务 id 或特定 shot id
	DependsOn     string   `json:"depends_on,omitempty"`
	DependsOnList []string `json:"depends_on_list,omitempty"`
}

type ShotGenerationParameters struct {
	// 用于 project_text 任务或 project 保存的默认参数
	ShotCount int    `json:"shot_count,omitempty"`
	Style     string `json:"style,omitempty"`
	StoryText string `json:"storyText"`
}

// 单个 shot 的生成参数（用于 shot_image 任务或单 shot 的默认参数）
type TaskShotParameters struct {
	ShotId      string `json:"shotId,omitempty"`
	Prompt      string `json:"prompt,omitempty"` // 分镜文本/提示
	Transition  string `json:"transition"`
	ImageWidth  int    `json:"image_width,omitempty"`
	ImageHeight int    `json:"image_height,omitempty"`
}

// TTS 参数（整片或单 shot 可复用）
type TaskTTSParameters struct {
	Voice      string `json:"voice,omitempty"`
	Lang       string `json:"lang,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	Format     string `json:"format,omitempty"`
}

type VideoParameters struct {
	Resolution string `json:"resolution,omitempty"` // e.g. "1920x1080"
	FPS        int    `json:"fps,omitempty"`
	Format     string `json:"format,omitempty"`  // e.g. "mp4"
	Bitrate    int    `json:"bitrate,omitempty"` // kbps
	// 可添加更多渲染/转码参数
}

// TaskResult 仅保留最小资源定位信息
type TaskResult struct {
	ResourceType string `json:"resource_type,omitempty"` // "project","shot","video","audio"
	ResourceID   string `json:"resource_id,omitempty"`   // 对应资源 id（由资源 API 提供详细信息）
	ResourceURL  string `json:"c,omitempty"`  // 可选直接访问的 URL（若系统直接提供）
}

// ...existing code...
func (t *Task) UpdateStatus(db *gorm.DB, status string, result interface{}, errMsg string) error {
	// 保持现有实现
	updates := map[string]interface{}{
		"status":     status,
		"updated_at": time.Now(),
	}
	if result != nil {
		updates["result"] = result
	}

	if errMsg != "" {
		updates["error"] = errMsg
	}
	return db.Model(t).Updates(updates).Error
}

func GetTaskByIDGorm(db *gorm.DB, taskID string) (*Task, error) {
	var task Task
	if err := db.First(&task, "id = ?", taskID).Error; err != nil {
		return nil, err
	}
	return &task, nil
}
