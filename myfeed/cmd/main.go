package main

import (
	"fmt"
	"log"

	"myfeed/internal/config"
	"myfeed/internal/db"

	"github.com/gin-gonic/gin"
)

func main() {
	// 1. 读配置
	cfg, err := config.Load("configs/config.yaml")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	log.Printf("config: %+v", cfg) // 调试用：亲眼确认 yaml 解析结果，阶段0验收完可删

	// 2. 连数据库
	gormDB, err := db.NewDB(cfg.Database)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}

	// 3. 建表
	if err := db.AutoMigrate(gormDB); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	// 4. 程序退出前关连接池
	defer func() {
		if err := db.CloseDB(gormDB); err != nil {
			log.Printf("关闭数据库失败: %v", err)
		}
	}()

	// 5. 起 HTTP 服务
	r := gin.Default()
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{"message": "pong"})
	})

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("server listening on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("启动服务失败: %v", err)
	}
}
