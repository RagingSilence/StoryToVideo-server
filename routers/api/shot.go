// ...existing code...
package api

import (
	"log"
	"net/http"
	"time"

	"StoryToVideo-server/models"
	"StoryToVideo-server/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 获取分镜列表
func GetShots(c *gin.Context) {
	projectID := c.Param("project_id")

	shots, err := models.GetShotsByProjectID(projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取分镜失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"shots":       shots,
		"project_id":  projectID,
		"total_shots": len(shots),
	})
}

// 更新分镜并可触发重生任务（改为使用新 TaskType）
func UpdateShot(c *gin.Context) {
	projectID := c.Param("project_id")
	shotID := c.Param("shot_id")

	var req struct {
		Title      string `form:"title" json:"title"`
		Prompt     string `form:"prompt" json:"prompt"`
		Transition string `form:"transition" json:"transition"`
	}
	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 确保分镜存在
	if _, err := models.GetShotByID(projectID, shotID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "分镜未找到: " + err.Error()})
		return
	}

	// 更新分镜数据库字段（只更新非空参数）
	if err := models.UpdateShotByID(projectID, shotID, req.Title, req.Prompt, req.Transition); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新分镜失败: " + err.Error()})
		return
	}

	// 创建重新生成分镜的任务（使用 models.TaskTypeShotImage）
	// Prompt 优先使用请求中的值，否则使用数据库中已有值（调用方已确保存在）
	prompt := req.Prompt

	task := models.Task{
		ID:        uuid.NewString(),
		ProjectId: projectID,
		Type:      models.TaskTypeShotImage,
		Status:    models.TaskStatusPending,
		Progress:  0,
		Message:   "分镜更新并已创建生成任务",
		Parameters: models.TaskParameters{
			Shot: models.TaskShotParameters{
				ShotId:      shotID,
				Prompt:      prompt,
				ImageWidth:  1024,
				ImageHeight: 1024,
			},
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "任务入队失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"shot_id": shotID,
		"task_id": task.ID,
		"message": "分镜已更新并创建生成任务",
	})
}

// 获取分镜详情
func GetShotDetail(c *gin.Context) {
	projectID := c.Param("project_id")
	shotID := c.Param("shot_id")

	shot, err := models.GetShotByID(projectID, shotID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "分镜未找到: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"shot": shot,
	})
}

// 删除分镜
func DeleteShot(c *gin.Context) {
	projectID := c.Param("project_id")
	shotID := c.Param("shot_id")

	// 如果路由未提供 project_id，则尝试按 shot_id 删除（直接执行 SQL）
	if projectID == "" {
		if _, err := models.DB.Exec(`DELETE FROM shot WHERE id = ?`, shotID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除分镜失败: " + err.Error()})
			return
		}
	} else {
		if err := models.DeleteShotByID(projectID, shotID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除分镜失败: " + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "分镜已删除",
		"shot_id":    shotID,
		"project_id": projectID,
	})
}

// 触发整片视频生成任务（创建 task 并入队）
func GenerateShotVideo(c *gin.Context) {
	projectID := c.Param("project_id")

	task := models.Task{
		ID:        uuid.NewString(),
		ProjectId: projectID,
		Type:      models.TaskTypeProjectVideo,
		Status:    models.TaskStatusPending,
		Progress:  0,
		Message:   "项目视频生成任务已创建",
		Parameters: models.TaskParameters{
			Video: models.VideoParameters{}, // 可由请求或 project 默认覆盖
		},
		Result:            models.TaskResult{},
		Error:             "",
		EstimatedDuration: 0,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	if err := models.CreateTask(&task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建视频任务失败: " + err.Error()})
		return
	}

	if err := service.EnqueueTask(task.ID); err != nil {
		log.Printf("视频任务入队失败: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "视频生成任务已创建",
		"project_id": projectID,
		"task_id":    task.ID,
	})
}

// ...existing code...
