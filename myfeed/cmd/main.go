package main

import (
	"fmt"
	"log"

	"myfeed/internal/config"
	"myfeed/internal/db"
	myhttp "myfeed/internal/http"
)

func main() {
	// 1. 读配置（相对路径：必须 cd myfeed 再 go run ./cmd）
	cfg, err := config.Load("configs/config.yaml")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 2. 连数据库
	gormDB, err := db.NewDB(cfg.Database)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}

	// 3. 建表（每完成一个模块，AutoMigrate 里加一张表）
	if err := db.AutoMigrate(gormDB); err != nil {
		log.Fatalf("自动建表失败: %v", err)
	}

	// 4. 程序退出前关连接池
	defer func() {
		if err := db.CloseDB(gormDB); err != nil {
			log.Printf("关闭数据库失败: %v", err)
		}
	}()

	// 5. 组装路由（依赖注入汇合点）并启动 HTTP 服务
	// 本轮起要往 router 传 cfg 了：检索的 depth / rrf_k 是纯配置，
	// 没法从 db 派生出来（见 router.go 顶部注释）
	r := myhttp.SetRouter(gormDB, cfg)
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("server listening on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("启动服务失败: %v", err)
	}
}
