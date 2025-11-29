// ...existing code...
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
	"log"

	"StoryToVideo-server/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"StoryToVideo-server/service"
) //119.45.124.222 //localhost

// 创建项目
func CreateProject(c *gin.Context) {
	var req struct {
		Title       string `form:"Title"`
		StoryText   string `form:"StoryText"`
		Style       string `form:"Style"`
		Description string `form:"Description"`
	}
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	project := models.Project{
		ID:          uuid.NewString(),
		Title:       req.Title,
		StoryText:   req.StoryText,
		Style:       req.Style,
		Description: req.Description,
		Status:      "created",
		CoverImage:  "",
		Duration:    0,
		VideoUrl:    "",
		ShotCount:   0,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// 持久化到数据库
	if err := models.CreateProject(&project); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建项目失败: " + err.Error()})
		return
	}

	// 创建任务（示例，实际逻辑可根据业务调整）
	task := models.Task{
		ID:        uuid.NewString(),
		ProjectId: project.ID,
		ShotId:    "",
		Type:      "create_project",
		Status:    "pending",
		Progress:  0,
		Message:   "项目创建任务已创建",
		Parameters: models.TaskParameters{
			Shot:  models.TaskShotParameters{},
			Video: models.TaskVideoParameters{},
		},
		Result:            models.TaskResult{},
		Error:             "",
		EstimatedDuration: 0,
		StartedAt:         time.Time{},
		FinishedAt:        time.Time{},
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	if err := models.CreateTask(&task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建任务失败: " + err.Error()})
		return
	}
	if err := service.EnqueueTask(task.ID); err != nil {
        log.Printf("任务入队失败: %v", err)
	}
	

	c.JSON(http.StatusOK, gin.H{
		"ProjectID": project.ID,
		"TaskID":    task.ID,
		"Status":    task.Status,
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
		if shotIDNull.Valid {
			t.ShotId = shotIDNull.String
		} else {
			t.ShotId = ""
		}
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
