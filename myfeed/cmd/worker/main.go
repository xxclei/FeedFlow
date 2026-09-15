// cmd/worker 是**第二个进程**。阶段9 之前整个项目只有一个 cmd/main.go，
// 从这一轮起有了两个可执行文件，各自独立启动、独立重启、独立伸缩。
//
// ---------- 为什么要拆两个进程 ----------
//
// API 进程的职责是**低延迟响应**：MQ 发布对它来说只是一次 marshal
// 加一次 socket 写（实测 publish 调用耗时 ≈ 0s，见
// middleware/rabbitmq/topology_test.go 末尾的时序说明）。
// 真正重的活 —— 开事务、写 MySQL、更新 Redis —— 全在这个进程里。
//
// 拆开之后两边各自拥有对方没有的能力：
//
//	worker 崩了 → API 照常收发请求，消息在队列里静静排队等它回来
//	API   崩了 → 队列里的消息照样被消费，数据不会因为接口挂了而丢失
//	流量涨了   → 只加 worker 实例（多开一个就多一份消费能力），API 不动
//
// 如果都挤在一个进程里，上面三条一条都做不到：worker 的重活会把
// API 的响应时间拖进同一个坑，而"多开几个实例"也会把 API 一起复制。
//
// ---------- 怎么跑 ----------
//
//	cd myfeed && go run ./cmd/worker
//
// 和 cmd/main.go 一样：**必须先 cd myfeed**，因为配置用的是相对路径。
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"myfeed/internal/config"
	"myfeed/internal/db"
	rabbitmq "myfeed/internal/middleware/rabbitmq"
	rediscache "myfeed/internal/middleware/redis"
	"myfeed/internal/social"
	"myfeed/internal/video"
	"myfeed/internal/worker"

	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	cfg, err := config.Load("configs/config.yaml")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// ---------- 必选依赖：MySQL ----------
	//
	// 和 API 进程一样是 log.Fatalf。判断标准是"没有它这个进程还有没有意义"：
	// worker 的全部工作就是往 MySQL 里写，连不上库它就是个空转的循环。
	gormDB, err := db.NewDB(cfg.Database)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}
	defer func() {
		if err := db.CloseDB(gormDB); err != nil {
			log.Printf("关闭数据库失败: %v", err)
		}
	}()

	// ---------- 可选依赖：Redis ----------
	//
	// 和 API 进程不同，这里**不是**"慢一点但功能齐全"：
	// cache == nil 时 PopularityWorker（唯一需要 Redis 的消费者）
	// 直接不启动 —— 它会一直失败重试然后丢弃消息，不如不跑。
	// 其余三个 worker 一个都不受影响（它们只写 MySQL）。
	cache, err := rediscache.NewFromEnv(&cfg.Redis)
	if err != nil {
		log.Printf("Redis 配置错误（热度 worker 将不启动）: %v", err)
		cache = nil
	} else {
		pingCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := cache.Ping(pingCtx); err != nil {
			log.Printf("Redis 不可用（热度 worker 将不启动）: %v", err)
			_ = cache.Close()
			cache = nil
		} else {
			defer cache.Close()
			log.Printf("Redis 已连接")
		}
	}

	// ---------- 必选依赖：RabbitMQ ----------
	//
	// 注意和 Redis 的对比：Redis 挂了降级，MQ 挂了**直接退出**。
	// 因为"worker 连不上 MQ"等于这个进程没有任何存在意义 ——
	// 它没有消费者可跑，也没有队列可声明。让一个空转进程活着，
	// 只会在监控上伪装成"worker 在跑"，比干脆退出来更难排查。
	//
	// 但**重试 10 次再退出**而不是立刻退出：MQ 容器可能比 worker
	// 晚启动几秒（docker compose 的经典问题），立刻死掉会变成
	// "重启一次就好"的玄学故障。
	var base *rabbitmq.RabbitMQ
	connectWithRetry("RabbitMQ", 10, func() error {
		b, err := rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
		if err != nil {
			return err
		}
		base = b
		return nil
	})
	defer func() {
		if err := base.Close(); err != nil {
			log.Printf("关闭 RabbitMQ 失败: %v", err)
		}
	}()

	// ---------- 声明拓扑 ----------
	//
	// worker 进程**一个 MQ 发布封装都不建**（它从不发布），所以这里
	// 直接用 DeclareAllTopology 一次声明**全部**业务链路，声明完就把这条临时
	// channel 关掉 —— 拓扑长在 broker 上，不依赖任何客户端连接。
	//
	// 为什么 worker 也要声明一遍（API 进程明明已经声明过了）：
	// **它不能假设 API 先启动**。worker 单独重启、或者 compose 里
	// worker 先起来时，如果它不自己声明，Consume 一个不存在的队列会
	// 直接报 404 NOT_FOUND —— 而且那个报错长得像"队列名写错了"。
	// declare 是幂等的，谁先谁后都对。
	topoCh, err := base.NewChannel()
	if err != nil {
		log.Fatalf("开拓扑声明 channel 失败: %v", err)
	}
	if err := rabbitmq.DeclareAllTopology(topoCh); err != nil {
		log.Fatalf("声明拓扑失败: %v", err)
	}
	_ = topoCh.Close()
	// 条数**从包的权威清单里取**，不写死。加转码那条链路时这句话曾经
	// 落后了一个月（写着 5 实际声明 6）—— 这种日志不会让任何东西出错，
	// 但它正好是排查时用来判断"拓扑是不是全建起来了"的那一行。
	// 一个会撒谎的探针比没有探针更坏。
	log.Printf("拓扑已声明（%d 条链路 + 死信队列）", rabbitmq.BusinessLinkCount())

	// ---------- 组装 repo ----------
	//
	// 注意 commentRepo 那条**跨模块依赖**：它来自 video 包，但这里
	// CommentWorker 用它，SocialWorker 用的是 social 包的 repo。
	// 四个 worker 的依赖没有共同形状 —— 它们唯一的共同点是"都是消费端"，
	// 这正是 worker 包按"是不是消费端"分包、而不是按"跑在哪"分包的原因。
	likeRepo := video.NewLikeRepository(gormDB)
	videoRepo := video.NewVideoRepository(gormDB)
	commentRepo := video.NewCommentRepository(gormDB)
	socialRepo := social.NewSocialRepository(gormDB)

	// ---------- 信号：Ctrl+C / SIGTERM 触发优雅退出 ----------
	//
	// NotifyContext 返回的 ctx 一旦被取消，所有 worker 的 Run 都会
	// 从 select 里跳出来 → 外层 runWorkerWithRetry 关掉 channel → 退出。
	// 关键是**正在处理的那条消息不会被腰斩**：handleDelivery 在退出时
	// 走的是 Nack(requeue=true)，消息回到队列交给下一个消费者。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---------- 起消费者 ----------
	//
	// 每个 worker 一个 goroutine、一条独立 channel。
	// **amqp.Channel 不是线程安全的**，所以"一个 worker 一条 channel"
	// 不是风格问题，是硬约束 —— 共用的后果是帧交错，表现为
	// 随机某条消息 Ack 到了另一条上。
	//
	// 本轮接全部四个消费者（点赞、评论、关注、热度）。
	//
	// 前三个的共同点：**一次用户操作 = 两条消息 = 两个不同的消费者**。
	// 点赞发 like + popularity，评论发 comment + popularity。
	// 两条腿互不依赖（一条只写 MySQL、一条只写 Redis），所以缺任何一个，
	// 功能都是**半坏不坏**的 —— 而且是从不同角度坏：
	//
	//	缺 LikeWorker        → 点赞永远不落库，用户刷新后赞就没了
	//	缺 PopularityWorker  → 点赞落库了，但热榜永远不涨（最阴的一种，
	//	                       因为落库那条路看着完全正常）
	//
	// 所以本文件末尾对 PopularityWorker 有个"Redis 不可用就不启动"的特殊处理，
	// 而其余三个没有 —— 它们不依赖 Redis。
	go runWorkerWithRetry(ctx, "LikeWorker", base, func(ch *amqp.Channel) error {
		return worker.NewLikeWorker(ch, likeRepo, videoRepo, likeQueueName).Run(ctx)
	})

	go runWorkerWithRetry(ctx, "CommentWorker", base, func(ch *amqp.Channel) error {
		return worker.NewCommentWorker(ch, commentRepo, videoRepo, commentQueueName).Run(ctx)
	})

	// SocialWorker 是四个里唯一**不写"真相"**的 —— 范式 B 下关注关系
	// 早已被请求侧同步写进库了，它消费到的每条 follow 都会撞 1062。
	// 它存在的意义是"把队列吃干净"，详见 internal/worker/social_worker.go
	// 顶上那段（那是一段不太好看的实话，值得一读）。
	go runWorkerWithRetry(ctx, "SocialWorker", base, func(ch *amqp.Channel) error {
		return worker.NewSocialWorker(ch, socialRepo, socialQueueName).Run(ctx)
	})

	// PopularityWorker **只在 Redis 可用时才启动**。
	//
	// cache == nil 时启动它会怎样：consumer 照常收消息、UpdatePopularityCache
	// 内部第一行 `if cache == nil { return }` 直接跳过、process 返回 nil、
	// 消息被正常 Ack —— 也就是说**消息会被静默吃掉**，一条不剩。
	// 队列看着是空的、日志一条错都没有，但热度全丢了。
	//
	// 不启动它，消息就堆在队列里等着 —— 等 Redis 修好、worker 重启，
	// 积压的消息会一股脑补进去。**一个静默丢失，一个原地等待**，
	// 显然后者才是想要的。
	if cache != nil {
		go runWorkerWithRetry(ctx, "PopularityWorker", base, func(ch *amqp.Channel) error {
			return worker.NewPopularityWorker(ch, cache, popularityQueueName).Run(ctx)
		})
	} else {
		log.Printf("Redis 不可用，PopularityWorker 不启动（热度消息将积压在队列里等待）")
	}

	// TranscodeWorker（扩展，无阶段编号）转码多档 HLS。
	//
	// 它是六个消费者里唯一一个**任务耗时以分钟计**的，所以有三处不同：
	// Qos(1)、不做优雅等待、失败即终态。三条的完整推导在
	// internal/worker/transcode_worker.go 开头。
	//
	// 这里额外说一件事：**它的失败不影响任何人看得见的东西**。
	// 转码失败 → transcode_status='failed' → 播放端落回直传原文件
	// （见 entity.go 里 hls_url 的说明）。所以这个 worker 连不上、
	// 或者 ffmpeg 没装，最坏的后果是"高码率视频还是原样播"，
	// 而不是"视频发不出去"。**这让它可以和其余五个并列启动，
	// 不需要像 PopularityWorker 那样先判依赖** —— 它的依赖缺失
	// 只降级它自己。
	go runWorkerWithRetry(ctx, "TranscodeWorker", base, func(ch *amqp.Channel) error {
		return worker.NewTranscodeWorker(ch, videoRepo, cache, cfg.Storage, cfg.Transcode, transcodeQueueName).Run(ctx)
	})

	<-ctx.Done()
	log.Printf("Worker shutting down...")

	// 这 2 秒给在途消息收尾（handleDelivery 里最多重试 4 次，但正常
	// 情况早就处理完了）。**它不是可靠性保证** —— 保证来自手动 Ack：
	// 没 Ack 的消息在 channel 关闭后会被 broker 重新投递。
	// 这 2 秒只是让大多数消息"不用重投"，省一次往返。
	time.Sleep(2 * time.Second)
	log.Printf("Worker stopped")
}

// 下面四个队列名必须和 rabbitmq 包里对应的常量一致。
//
// ⚠ 这里是**手写的字面量**，和 rabbitmq 包内那两个常量是两个地方 ——
// 这是刻意的重复，不是疏忽：cmd/worker 是 main 包，
// 它 import 不到 unexported 的 likeQueue / popularityQueue。
//
// 漂移的后果很隐蔽：队列名写错 → Consume 一个不存在的队列 → 404。
// 好在这个错**会立刻炸**（不像消息发错地方那样静默），所以可以接受。
// 真正要警惕的是语义相同、名字不同的那种漂移。
const (
	likeQueueName = "like.events"

	// ⚠ 这条**不叫** video.popularity.update —— 那是 routing key。
	// 队列名和交换机名一样都是 video.popularity.events（五条链路里
	// 只有 timeline 那条例外，见 topology.go）。
	popularityQueueName = "video.popularity.events"

	commentQueueName = "comment.events"
	socialQueueName  = "social.events"

	// 转码（扩展）。⚠ **这是唯一一条队列名和交换机名都带 `.queue` 的链路**
	// （其余五条 Queue = Exchange），见 topology.go 里 timeline 那条的同类说明。
	// 手抄错的表现和前四条一样：Consume 一个不存在的队列 → 启动时 404，立刻炸。
	transcodeQueueName = "video.transcode.queue"
)

// connectWithRetry 指数退避重试，退避 1s/2s/4s/...封顶 30s。
//
// 封顶 30s 是必须的：不封顶的话 1<<i 在 i=10 时就是 1024 秒，
// 重试 10 次的总时长会变成几小时 —— 那时候容器编排早就把进程判成
// 不健康杀掉了，"重试"也就失去了意义。
func connectWithRetry(name string, maxRetries int, fn func() error) {
	for i := 0; i < maxRetries; i++ {
		if err := fn(); err == nil {
			log.Printf("[%s] 连接成功（第 %d 次尝试）", name, i+1)
			return
		} else {
			log.Printf("[%s] 连接失败（第 %d 次尝试）: %v", name, i+1, err)
		}

		backoff := time.Duration(1<<uint(i)) * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		log.Printf("[%s] %v 后重试...", name, backoff)
		time.Sleep(backoff)
	}
	log.Fatalf("[%s] 重试 %d 次后仍连不上, 退出", name, maxRetries)
}

// runWorkerWithRetry 是**断线重连**那一层，和 handleDelivery 的
// **消息重试**那一层是两件完全不同的事：
//
//	handleDelivery   同一条消息试 4 次，管的是"这条消息处理失败"
//	runWorkerWithRetry 整个消费者重建 channel，管的是"这条连接断了"
//
// 分开的理由：连接的生死和单条消息的生死没有因果关系。
// 混在一起写的话，会出现"因为一条消息是毒消息，把整条 channel
// 重连了 4 次"这种荒唐行为。
func runWorkerWithRetry(ctx context.Context, name string, base *rabbitmq.RabbitMQ, fn func(*amqp.Channel) error) {
	for {
		if ctx.Err() != nil {
			return
		}

		ch, err := base.NewChannel()
		if err != nil {
			log.Printf("[%s] 开 channel 失败, 5 秒后重试: %v", name, err)
			time.Sleep(5 * time.Second)
			continue
		}

		// prefetch=50：**背压**。没有它 broker 会把队列里的消息
		// 一股脑推给消费者，全堆在客户端内存里 —— 队列看着是空的，
		// 实际压力全转嫁到了 worker 进程，而且一崩就是几万条一起重投。
		// 50 的意思是"我手上最多同时有 50 条未 Ack 的"，
		// 处理完一条才拿新的。超过 50 的还在 broker 那边等着，
		// 那才是它该待的地方。
		//
		// Qos 失败只记日志不中断：它是个优化，不是功能。
		// 退化成无背压也能跑，不值得为它把连接重来一遍。
		if err := ch.Qos(50, 0, false); err != nil {
			log.Printf("[%s] 设置 Qos 失败（继续，无背压）: %v", name, err)
		}

		log.Printf("[%s] started, consuming", name)
		err = fn(ch)

		// 退出路径要分清：ctx 取消是**正常退出**，不该重连。
		// 不加这个判断的话，Ctrl+C 之后进程会一边关一边重连，
		// 看起来像"停不下来"。
		if ctx.Err() != nil {
			_ = ch.Close()
			return
		}

		log.Printf("[%s] 消费者退出, 5 秒后重连: %v", name, err)
		_ = ch.Close()
		time.Sleep(5 * time.Second)
	}
}
