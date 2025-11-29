// ...existing code...
package api

import (
	"net/http"
	"time"

	"testgin/models"

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

// 生成分镜
// func CreateShot(c *gin.Context) {
//     projectID := c.Param("project_id")
//     var req struct {
//         Title      string `form:"title"`
//         Prompt     string `form:"prompt"`
//         Transition string `form:"transition"`
//     }
//     if err := c.ShouldBindQuery(&req); err != nil {
//         c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
//         return
//     }

//     // 计算顺序（简单方式：读取当前分镜数量）
//     existingShots, err := models.GetShotsByProjectID(projectID)
//     if err != nil {
//         c.JSON(http.StatusInternalServerError, gin.H{"error": "读取分镜信息失败: " + err.Error()})
//         return
//     }
//     order := len(existingShots) + 1

//     shot := models.Shot{
//         ID:          uuid.NewString(),
//         ProjectId:   projectID,
//         Order:       order,
//         Title:       req.Title,
//         Description: "",
//         Prompt:      req.Prompt,
//         Status:      "created",
//         ImagePath:   "",
//         AudioPath:   "",
//         Transition:  req.Transition,
//         CreatedAt:   time.Now(),
//         UpdatedAt:   time.Now(),
//     }

//     // 持久化到数据库
//     if err := models.CreateShot(&shot); err != nil {
//         c.JSON(http.StatusInternalServerError, gin.H{"error": "创建分镜失败: " + err.Error()})
//         return
//     }

//     // 为该分镜创建生成任务
//     task := models.Task{
//         ID:        uuid.NewString(),
//         ProjectId: projectID,
//         ShotId:    shot.ID,
//         Type:      "generate_shot",
//         Status:    "pending",
//         Progress:  0,
//         Message:   "分镜生成任务已创建",
//         Parameters: models.TaskParameters{
//             Shot: models.TaskShotParameters{
//                 Style:       "",              // 可根据业务填充
//                 TextLLM:     shot.Prompt,
//                 ImageLLM:    "",
//                 GenerateTTS: false,
//                 ShotCount:   1,
//                 ImageWidth:  1024,
//                 ImageHeight: 1024,
//             },
//             Video: models.TaskVideoParameters{},
//         },
//         Result:            models.TaskResult{},
//         Error:             "",
//         EstimatedDuration: 0,
//         StartedAt:         time.Time{},
//         FinishedAt:        time.Time{},
//         CreatedAt:         time.Now(),
//         UpdatedAt:         time.Now(),
//     }

//     if err := models.CreateTask(&task); err != nil {
//         c.JSON(http.StatusInternalServerError, gin.H{"error": "创建任务失败: " + err.Error()})
//         return
//     }

//	    c.JSON(http.StatusOK, gin.H{
//	        "shot_id": shot.ID,
//	        "task_id": task.ID,
//	        "message": "分镜生成任务已创建",
//	    })
//	}
func UpdateShot(c *gin.Context) {
	projectID := c.Param("project_id")
	shotID := c.Param("shot_id")

	var req struct {
		Title      string `form:"title"`
		Prompt     string `form:"prompt"`
		Transition string `form:"transition"`
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

	// 创建重新生成分镜的任务（示例：type 为 regenerate_shot）
	task := models.Task{
		ID:        uuid.NewString(),
		ProjectId: projectID,
		ShotId:    shotID,
		Type:      "regenerate_shot",
		Status:    "pending",
		Progress:  0,
		Message:   "分镜更新并已创建生成任务",
		Parameters: models.TaskParameters{
			Shot: models.TaskShotParameters{
				Style:       "", // 可按需填充
				TextLLM:     req.Prompt,
				ImageLLM:    "",
				GenerateTTS: false,
				ShotCount:   1,
				ImageWidth:  1024,
				ImageHeight: 1024,
			},
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

// 视频生成
func GenerateShotVideo(c *gin.Context) {
	projectID := c.Param("project_id")
	// TODO: 触发视频生成任务（可在此处创建 Task 并持久化）
	c.JSON(http.StatusOK, gin.H{
		"message":    "视频生成任务已创建",
		"project_id": projectID,
		"task_id":    uuid.NewString(), // TODO: 实际任务ID
	})

}

// ...existing code...
