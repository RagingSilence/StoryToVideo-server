package main

import (
	"fmt"
	"testgin/config"
	"testgin/models"
	"testgin/routers"
)

func main() {
	config.InitConfig()
	fmt.Println("Server starting on port", config.AppConfig.Server.Port)
	models.InitDB()
	fmt.Println("Database initialized")
	r := routers.InitRouter()
	r.Run(config.AppConfig.Server.Port)
}
