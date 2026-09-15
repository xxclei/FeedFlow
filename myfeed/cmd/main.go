package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"myfeed/internal/config"
	"myfeed/internal/db"
	myhttp "myfeed/internal/http"
	rabbitmq "myfeed/internal/middleware/rabbitmq"
	rediscache "myfeed/internal/middleware/redis"
)

// shutdownGrace 优雅关闭最多等多久。
//
// 15 秒的取值要连着上面 ④ 一起看：**SSE 挂着时这个等待必然走满**，
// 所以它同时是"每次部署会花多久"的上界。选 15 而不是 30/60：
// 容器编排（含 docker compose 默认的 10 秒 SIGKILL 之前）留的余量本来就不多，
// 而这个进程在关闭时真正需要收尾的只有三个连接池的 Close（毫秒级）。
// 与其为了一个不会自己结束的 SSE 连接等更久，不如把时间花在"下次部署"上。
const shutdownGrace = 15 * time.Second

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

	// 5. 连 Redis（可选依赖：连不上只记日志、置 nil，服务照常启动）。
	//
	// 这是**启动降级**：和上面 DB 的 log.Fatalf 形成对比 ——
	// DB 是必选依赖（没有它整个项目无意义），Redis 是可选加速件
	// （没有它只是慢，功能一条不缺）。所以这里失败绝不退出进程。
	//
	// cache==nil 会一路传给 jwt/account/feed 等所有用它的地方，
	// 封装层的 nil 接收器保护 + service 的 `if cache != nil` 双保险，
	// 保证全链路都能走 DB 兜底。详见 internal/middleware/redis/redis.go。
	cache, err := rediscache.NewFromEnv(&cfg.Redis)
	if err != nil {
		log.Printf("Redis 配置错误（缓存禁用）: %v", err)
		cache = nil
	} else {
		// NewFromEnv 是懒连接，几乎不会返回 err（构造 client 不碰网络）。
		// 真正的"连不上"只在 Ping 时暴露 —— 所以这个 300ms 超时的 Ping
		// 是启动降级的唯一判定点。
		pingCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := cache.Ping(pingCtx); err != nil {
			log.Printf("Redis 不可用（缓存禁用）: %v", err)
			_ = cache.Close()
			cache = nil
		} else {
			defer cache.Close()
			log.Printf("Redis 已连接（缓存启用）")
		}
	}

	// 5.5 连 RabbitMQ（可选依赖，和 Redis 同一个套路：连不上置 nil，服务照常启动）。
	//
	// 位置在 Redis 之后、组装路由之前。**顺序不敏感**，两个都是可选依赖，
	// 谁先谁后只影响日志里哪一条先出现。
	//
	// 这里比 Redis 多一层：NewRabbitMQ 内部是 amqp.Dial，**它是真会碰网络的**
	// （不像 redis.NewClient 是懒连接，几乎不返回 err）。所以这个 err
	// 是实打实的"连不上"，不是配置写错了。
	//
	// 连不上会怎样：五个 MQ 封装全部置 nil，五个 service 全部走降级路径 ——
	// 点赞同步写 MySQL、同步写 Redis。功能一条不缺，只是回到阶段8 的形态。
	// 这就是"MQ 是加速件不是必需品"这句话在启动代码里的样子。
	var rmq *rabbitmq.RabbitMQ
	rmq, err = rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
	if err != nil {
		log.Printf("RabbitMQ 不可用（MQ 降级为同步直写）: %v", err)
		rmq = nil
	} else {
		defer func() {
			if err := rmq.Close(); err != nil {
				log.Printf("关闭 RabbitMQ 失败: %v", err)
			}
		}()
		log.Printf("RabbitMQ 已连接（MQ 启用）")
	}

	// 6. 组装路由（依赖注入汇合点）并启动 HTTP 服务。
	// 本轮起要往 router 传 cache 和 rmq 了：两个都可能为 nil（启动降级），
	// 由 router 注入到 jwt 中间件和各 service。
	// 检索参数（depth / rrf_k）也是纯配置，没法从 db 派生（见 router.go 顶部注释）
	r := myhttp.SetRouter(gormDB, cfg, cache, rmq)
	addr := fmt.Sprintf(":%d", cfg.Server.Port)

	// ---------- 7. 起服务，并接住 SIGTERM/SIGINT 做优雅关闭 ----------
	//
	// 为什么不能再用 `r.Run(addr)`：gin 的 Run 内部就是 http.ListenAndServe，
	// 而它**没有任何信号处理** —— SIGTERM 由 Go runtime 按默认行为处理 = 立即退出。
	// 容器里 `docker stop` / `docker compose down` / 每次重新部署发的都是 SIGTERM，
	// 于是"立即退出"是每天都会发生的事，不是边界情况。
	//
	// 换成 http.Server + Shutdown 之后，**这几件事真的修好了**：
	//
	//	① 上面三个 defer（31 行的关 DB / 61 行的关 Redis / 84 行的关 RabbitMQ）
	//	   会执行了。在此之前它们从不执行 —— 连接不是被优雅关闭，
	//	   是随进程一起消失的。这是本次改动最主要、最确定的一个收益。
	//	② 在途的 HTTP 请求有机会跑完，不会在半路被切断。
	//
	// ⚠ **没修好的两件事，一并写在这里，免得被当成"已经解决了"**：
	//
	//	③ 本进程内跑着的后台消费者和 outbox 轮询器（1 秒一轮）**仍然会被硬切**。
	//	   Shutdown 只管 HTTP 连接，管不到别的 goroutine；main 返回后进程退出，
	//	   它们跟着死。真正修它要把这个 ctx 传进 router、再传给每个 worker，
	//	   那是动 router 的构造签名，本轮不做。
	//	④ SSE 是长连接，**它不会自己结束**。所以只要有客户端挂着通知流，
	//	   下面每次都会等满 shutdownGrace 才强杀 —— 这是"多等 15 秒"，
	//	   不是"优雅断开"。想干净就得让 ssehub 订阅这个 ctx 主动关订阅者。
	//
	// 写法的依据：`cmd/worker/main.go:156` 一直是这么做的（signal.NotifyContext），
	// 这里照抄它的形态，两个进程的关闭语义保持一致。
	srv := &http.Server{Addr: addr, Handler: r}

	// NotifyContext 而不是手工 signal.Notify + channel：它把"收到信号"变成一个 ctx，
	// 下面就能和"起服务失败"并列塞进同一个 select，不用再手抄一遍信号通道。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 带缓冲的通道：起服务失败时我们可能已经走在 ctx.Done() 那一支上了，
	// 无缓冲的话那个 goroutine 会永远阻塞在发送上（goroutine 泄漏，
	// 虽然进程马上就退了所以看不出来 —— 但这不是"反正看不出来"就该写的代码）。
	serveErr := make(chan error, 1)
	go func() {
		// ErrServerClosed 是 Shutdown 的正常返回值，不是错误。不排掉的话
		// 每次优雅关闭都会多走一条"启动服务失败"的日志，误导性极强。
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	log.Printf("server listening on %s", addr)

	select {
	case err := <-serveErr:
		// 端口被占、没权限绑低端口、地址写错 —— 都走这里。
		// 仍然 Fatal：起不来就别假装起来了（容器编排会按退出码重试/告警）。
		log.Fatalf("启动服务失败: %v", err)
	case <-ctx.Done():
		log.Printf("收到停止信号，开始优雅关闭（最多等 %s）…", shutdownGrace)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// 超时不等于失败：SSE 挂着就一定会走到这里（见上面 ④）。
		// 记一条日志、继续往下走，让三个 defer 照常执行 ——
		// 连接池该关还是要关的。
		log.Printf("优雅关闭超时（有连接没在 %s 内结束，接下来强制退出）: %v", shutdownGrace, err)
	} else {
		log.Printf("HTTP 已优雅关闭")
	}
}
