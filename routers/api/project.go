// ...existing code...
package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"StoryToVideo-server/models"

	"StoryToVideo-server/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
) //119.45.124.222 //localhost

// 创建项目
func CreateProject(c *gin.Context) {
	var req struct {
		Title     string `form:"Title" json:"title"`
		StoryText string `form:"StoryText" json:"story_text"`
		Style     string `form:"Style" json:"style"`
		ShotCount int    `form:"ShotCount" json:"shot_count"`
	}
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 默认分镜数量
	if req.ShotCount <= 0 {
		req.ShotCount = 5
	}

	project := models.Project{
		ID:          uuid.NewString(),
		Title:       req.Title,
		StoryText:   req.StoryText,
		Style:       req.Style,
		Status:      "created",
		CoverImage:  "",
		Duration:    0,
		VideoUrl:    "",
		Description: "",
		ShotCount:   req.ShotCount,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// 1) 插入 project 到 DB
	if err := models.CreateProject(&project); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建项目失败: " + err.Error()})
		return
	}

	// 2) 创建项目文本生成任务（project_text）
	textTask := models.Task{
		ID:        uuid.NewString(),
		ProjectId: project.ID,
		ShotId:    "",
		Type:      models.TaskTypeStoryboard,
		Status:     models.TaskStatusPending,
		Progress:  0,
		Message:   "项目创建任务已创建,正在生成分镜脚本...",
		Parameters: models.TaskParameters{
			ShotDefaults: &models.ShotDefaultsParams{
				ShotCount: req.ShotCount,
				Style:     req.Style,
				StoryText: req.StoryText,
			},
			Shot:  &models.ShotParams{},
			Video: &models.VideoParams{},
			TTS:   &models.TTSParams{},
		},
		Result:            models.TaskResult{},
		Error:             "",
		EstimatedDuration: 0,
		StartedAt:         time.Time{},
		FinishedAt:        time.Time{},
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	if err := models.CreateTask(&textTask); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建文本任务失败: " + err.Error()})
		return
	}
	// 将文本任务入队执行
	if err := service.EnqueueTask(textTask.ID); err != nil {
		log.Printf("文本任务入队失败: %v", err)
	}

	// 3) 创建 n 个分镜图片生成任务，状态为 blocked，并设置依赖为 textTask.ID
	var shotTaskIDs []string
	for i := 0; i < req.ShotCount; i++ {
		shotTask := models.Task{
			ID:        uuid.NewString(),
			ProjectId: project.ID,
			Type:      models.TaskTypeShotImage,
			Status:    models.TaskStatusBlocked,
			Progress:  0,
			Message:   "等待文本任务完成以生成分镜图片",
			Parameters: models.TaskParameters{
				Shot: &models.ShotParams{
					Prompt:      "",
					Transition:  "",
					ImageWidth:  "1024",
					ImageHeight: "1024",
				},
				DependsOn: []string{textTask.ID},
			},
			Result:            models.TaskResult{},
			Error:             "",
			EstimatedDuration: 0,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		}
		if err := models.CreateTask(&shotTask); err != nil {
			log.Printf("创建分镜任务失败: %v", err)
			continue
		}
		shotTaskIDs = append(shotTaskIDs, shotTask.ID)
		// 不入队，等待依赖解锁 (文本任务完成后由 watcher 或处理器解锁并入队)
	}

	c.JSON(http.StatusOK, gin.H{
		"project_id":    project.ID,
		"text_task_id":  textTask.ID,
		"shot_task_ids": shotTaskIDs,
	})
}

// 获取项目详情
func GetProject(c *gin.Context) {
	projectID := c.Param("project_id")

	// 从数据库获取项目
	project, err := models.GetProjectByID(projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目未找到: " + err.Error()})
		return
	}

	// 获取分镜列表
	shots, err := models.GetShotsByProjectID(projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取分镜失败: " + err.Error()})
		return
	}

	// 获取最近任务（如果有）
	var recentTask *models.Task
	row := models.DB.QueryRow(`SELECT id, project_id, shot_id, type, status, progress, message, parameters, result, error, estimated_duration, started_at, finished_at, created_at, updated_at FROM task WHERE project_id = ? ORDER BY created_at DESC LIMIT 1`, projectID)
	var t models.Task
	var paramsBytes, resultBytes []byte
	var startedAt, finishedAt, createdAt, updatedAt sql.NullTime
	var shotIDNull sql.NullString
	var messageNull sql.NullString
	var errorNull sql.NullString

	if err := row.Scan(&t.ID, &t.ProjectId, &shotIDNull, &t.Type, &t.Status, &t.Progress, &messageNull, &paramsBytes, &resultBytes, &errorNull, &t.EstimatedDuration, &startedAt, &finishedAt, &createdAt, &updatedAt); err != nil {
		if err != sql.ErrNoRows {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询最近任务失败: " + err.Error()})
			return
		}
		// 没有任务，recentTask 保持 nil
	} else {
		if messageNull.Valid {
			t.Message = messageNull.String
		} else {
			t.Message = ""
		}
		if errorNull.Valid {
			t.Error = errorNull.String
		} else {
			t.Error = ""
		}
		// 反序列化 parameters/result
		_ = json.Unmarshal(paramsBytes, &t.Parameters)
		_ = json.Unmarshal(resultBytes, &t.Result)
		if startedAt.Valid {
			t.StartedAt = startedAt.Time
		}
		if finishedAt.Valid {
			t.FinishedAt = finishedAt.Time
		}
		if createdAt.Valid {
			t.CreatedAt = createdAt.Time
		}
		if updatedAt.Valid {
			t.UpdatedAt = updatedAt.Time
		}
		recentTask = &t
	}

	c.JSON(http.StatusOK, gin.H{
		"project_detail": project,
		"shots":          shots,
		"recent_task":    recentTask,
	})
}

// 更新项目信息
func UpdateProject(c *gin.Context) {
	projectID := c.Param("project_id")
	var req struct {
		Title       string `form:"Title"`
		Description string `form:"Description"`
	}
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 数据库更新项目
	if err := models.UpdateProjectByID(projectID, req.Title, req.Description); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新项目失败: " + err.Error()})
		return
	}

	updatedProject, err := models.GetProjectByID(projectID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"id":       projectID,
			"updateAT": time.Now(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"project":  updatedProject,
		"updateAT": updatedProject.UpdatedAt,
	})
}

// 删除项目
func DeleteProject(c *gin.Context) {
	projectID := c.Param("project_id")

	// 数据库删除项目（级联会删除相关分镜和任务）
	if err := models.DeleteProjectByID(projectID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除项目失败: " + err.Error()})
		return
	}

	deleteAt := time.Now()

	c.JSON(http.StatusOK, gin.H{
		"success":  true,
		"deleteAt": deleteAt,
		"message":  "项目已删除",
	})
}

// ...existing code...
