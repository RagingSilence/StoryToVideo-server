package main

import (
	"fmt"
	"StoryToVideo-server/config"
	"StoryToVideo-server/models"
	"StoryToVideo-server/routers"
	"StoryToVideo-server/service"
	"context"
)

func main() {
	config.InitConfig()
	fmt.Println("Server starting on port", config.AppConfig.Server.Port)
	models.InitDB()
	fmt.Println("Database initialized")

	if err := service.InitQueue(); err != nil {
        log.Fatal("Failed to initialize queue:", err)
    }
	processor := service.NewProcessor(models.GormDB)
	processor.StartProcessor(5)

	r := routers.InitRouter()
	r.Run(config.AppConfig.Server.Port)
}
