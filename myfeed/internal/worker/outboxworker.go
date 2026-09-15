// 本文件是 worker 包里**唯一跑在 API 进程**的那一半 —— 另一处例外是
// notification_worker.go / ssehub.go。它们和 like_worker 那几个的分界线
// 不是"跑在哪"，而是**"用户等不等它"**：
//
//	LikeWorker 等不等得起  → 等不起（那是点赞的落库，用户马上要看到）
//	                        → 但它也不在请求里，所以丢给独立进程，能慢慢来
//	outbox 轮询 / timeline 消费 / 通知推送 → **必须**在 API 进程
//
// 最后一条的理由是资源位置：**Redis 连接、SSE 连接（客户端挂在 API 上）、
// 内存里的 hub 都在 API 进程**。timeline 消费者要写 Redis，通知消费者要
// 往 SSE 长连接里塞数据 —— 这些资源没法跨进程共享。所以它们只能在 API 进程里，
// 用 goroutine 而不是新进程来并发。
//
// 这就解释了为什么"拆进程"这件事在这里停下来：**拆进程的前提是
// 搬过去的那个东西和原进程没有共享资源。** 一旦要共享（Redis 连接池、
// 长连接、内存注册表），拆过去就得改成 RPC，那是另一个量级的复杂度。
// 原项目停在这里是对的，值得记住这个边界在哪。
package worker

import (
	"context"
	"errors"
	"log"
	"time"

	"myfeed/internal/middleware/rabbitmq"
	rediscache "myfeed/internal/middleware/redis"
	"myfeed/internal/video"

	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// timelineKey 是全局时间线在 Redis 里的 ZSet key。
//
// ⚠ **这个名字必须和 feed/latest_cache.go 里那个保持一致** ——
// 一边写、一边读，写错方向不会报错，只会让 ListLatest 永远读到一个空 ZSet
// （然后静默回落到"从 MySQL 查最新"的兜底路径，性能特征变了但功能看着正常）。
//
// 这里用了 cache.Key() 前缀，所以字面量是 `feed:global_timeline`，
// 实际 key 是 `v1:feed:global_timeline`。**两边都要走 Key()**——
// 如果一边忘了，就会出现"写进 v1: 前缀的、读没前缀的"这种查一整天查不出来的错。
const timelineKey = "feed:global_timeline"

// timelineMaxSize 时间线只保留最新 1000 条。
//
// 这是"热区"的定义：ListLatest 的第一页从这里取，翻页翻深了会回落到 MySQL。
// 1000 是原项目的数字，本质上是**内存预算**的取舍 —— 每个 member 约几十字节，
// 1000 条不到 100KB，可以毫无心理负担地常驻。
//
// 增长会怎样：不裁剪的话 ZSet 会跟着视频总量线性涨，
// 而这个东西**没有任何 TTL**（它不是缓存，是索引），最后变成内存泄漏。
const timelineMaxSize = 1000

// outboxPollInterval 轮询间隔。文档给的是 1 秒。
//
// 这个 1 秒就是 outbox 模式的**可见性延迟下限**：视频发布后，最长要等
// 1 秒才会被中继进时间线。想更快只能缩短间隔，代价是空轮询的 SQL 变密 ——
// 每秒一次 `SELECT ... WHERE status='pending' LIMIT 100` 在空表上代价可忽略，
// 但缩到 10ms 就不一样了（100 倍的空查询）。
//
// 更好的做法不是缩间隔而是**事件驱动**：写 outbox 时发一个信号
// （channel / Redis pub-sub）唤醒轮询器。本项目不做，照原项目。
const outboxPollInterval = 1 * time.Second

// StartOutboxPoller 是事务性发件箱的**中继**那一半（另一半是 VideoService.Publish
// 里跟视频同事务写下的 pending 行）。
//
// 它解决的问题，一句话版本：
//
//		**"写数据库"和"发消息"是两件事，只要它们是分开的两次调用，
//		  中间就必然有一个会让两者不一致的崩溃点。**
//
//	  - 先写库再发 MQ：写完库崩了 → 视频发出去了，时间线里永远没有它（丢消息）
//	  - 先发 MQ 再写库：发完崩了 → 时间线里有一条指向不存在视频的 id（脏数据）
//
// outbox 的解法是：把"要发的消息"降级成**一行数据**，让它和业务数据
// 在同一个事务里落库。于是"业务成功 ⇔ 消息已记录"变成数据库自己保证的事
// （要么都提交，要么都回滚），**双写困境从"两次调用的一致性"变成了
// "一次事务的原子性"** —— 前者做不到，后者是数据库的本职工作。
//
// 剩下的事就是本函数：把 pending 的行搬到 MQ，搬成功了才删。
//   - 搬到一半崩了 → 行还在，下一轮重搬（**重发，不丢**）
//   - 搬成功了删失败 → 下一轮重搬一次（**重复，消费端幂等**）
//
// 两种崩溃都收敛，代价是"至少一次"的重复。这就是最终一致性。
//
// 降级：全部投递目标都不可用 → 直接 return，**轮询器不启动**。
// 于是 outbox 行会一直积压。这不是故障，这是**正确的降级形态**：
// 行在 MySQL 里躺着，一条不丢；等 MQ 恢复、API 重启，积压会被一次性补投。
// 对比"全 nil 也照跑，每轮把行删掉"—— 那才是灾难（静默丢光）。
//
// ⚠ 判据是"**全部**都不可用"而不是"timeline 不可用"（本轮从单目标改成多目标时
// 改的一处）。只挂一条链路失败就停掉整个轮询器的话，另一条链路的消息
// 会跟着一起积压 —— 而它们本来是能投出去的。**降级要按能用的部分降，
// 不是按坏了的部分停。**
func StartOutboxPoller(db *gorm.DB, pubs OutboxPublishers) {
	if pubs.Timeline == nil && pubs.Transcode == nil {
		log.Printf("[OutboxPoller] 所有投递目标都不可用, 轮询器不启动（outbox 行将持续积压, 恢复后补投）")
		return
	}
	if pubs.Timeline == nil {
		log.Printf("[OutboxPoller] ⚠ timelineMQ 不可用：video_published 类事件将积压（其余事件照常投递）")
	}
	if pubs.Transcode == nil {
		log.Printf("[OutboxPoller] ⚠ transcodeMQ 不可用：video_transcode 类事件将积压（其余事件照常投递）")
	}

	// ⚠ **这个 ctx 必须是 Background，绝不能是请求的 ctx。**
	//
	// 本项目最阴的一个坑（文档"常见的坑"点名过）：轮询器是后台 goroutine，
	// 它**根本没有请求**。如果实现时顺手写了个 ctx 参数并从某处传进来 ——
	// 或者更常见的是，把它写在某个 handler 里、顺手用了 c.Request.Context() ——
	// 那么请求一结束，轮询器第一次 publish 就返回 `context canceled`，
	// 纸条一条都投不出去。**而且那个错误长得像"MQ 坏了"**，会把人往
	// 交换机/队列/binding 上带，查半天查不到"ctx 已经取消了"这件事上。
	//
	// 判断标准可以记成一句：**决定任务生死的不能是那个注定会结束的东西。**
	// 请求的 ctx 注定会结束（用户总会断开），所以它不能决定一个
	// 生命周期跨越无数个请求的后台任务能活多久。
	ctx := context.Background()

	go func() {
		log.Printf("[OutboxPoller] 启动, 间隔 %v", outboxPollInterval)
		for {
			// 每轮都重取一次 ctx（这里就是 Background，但形状上留出
			// "将来要加超时"的位置）。注意**不是**在这里 select ctx.Done() ——
			// 轮询器的退出方式是**进程退出**，它没有优雅停止的需求：
			// 半途退出最坏就是少投一条，下一轮（或下次启动）会补上。
			pollOnce(ctx, db, pubs)
			time.Sleep(outboxPollInterval)
		}
	}()
}

// OutboxPublishers 轮询器能投递的全部目标。
//
// 做成一个结构体而不是给 StartOutboxPoller 加第二个参数，是因为
// **它只会继续变长**（每加一种事件类型就多一个目标），而参数列表
// 每变一次就要改所有调用点。结构体加字段不会。
//
// 两个字段都可以是 nil（各自的 MQ 声明失败 / 连不上 broker），
// 判空在 deliver 里做 —— 那里能给出"是哪个目标缺了"的具体错误，
// 而不是一个笼统的"投递失败"。
type OutboxPublishers struct {
	Timeline  *rabbitmq.TimelineMQ
	Transcode *rabbitmq.TranscodeMQ
}

// errNoPublisher 目标链路不存在。**不是毒消息** —— 行要保留，
// 等 MQ 恢复后重投（这正是 outbox 模式的意义）。
type errNoPublisher struct{ link string }

func (e errNoPublisher) Error() string { return e.link + " publisher 不可用" }

// errUnknownEventType 未知的 EventType。**是毒消息** —— 无论等多久、
// 重投多少次都不会变得可投递，所以不能像 errNoPublisher 那样留着。
type errUnknownEventType struct{ typ string }

func (e errUnknownEventType) Error() string { return "未知的 EventType: " + e.typ }

// deliver 按 EventType 把一条 outbox 行投到对应的链路。
//
// ---------- 这个 switch 是必须的，不是"可选的整理" ----------
//
// 在本函数出现之前，pollOnce 只按 `status='pending'` 查，然后**无条件**
// 调用 TimelineMQ.PublishVideo —— 它从不读 EventType。
//
// 那个状态下如果直接往 outbox 表里加一条 `video_transcode` 行，会发生：
//
//	① 它被同一段代码捞出来，当成"视频已发布"投进 **video.timeline.update.queue**
//	② 消费端把它 ZADD 进全局时间线 —— 一条 id 重复的时间线成员
//	③ 而真正的转码队列**永远不会收到这条消息**，视频永远停在 pending
//
// 三个后果没有一个会报错：转码队列是空的（没人发），时间线多一条重复
// （ZADD 同 member 幂等，看不出来），视频状态停在一个"看起来只是在排队"
// 的值上。**这是本轮改动里唯一一处"不改就静默出错"的地方。**
func (p OutboxPublishers) deliver(ctx context.Context, msg *video.OutboxMsg) error {
	switch msg.EventType {
	case video.EventTypeVideoPublished:
		if p.Timeline == nil {
			return errNoPublisher{"timeline"}
		}
		return p.Timeline.PublishVideo(ctx, msg.VideoID, msg.CreateTime)

	case video.EventTypeVideoTranscode:
		if p.Transcode == nil {
			return errNoPublisher{"transcode"}
		}
		return p.Transcode.PublishTranscode(ctx, msg.VideoID, msg.CreateTime)

	default:
		// 空字符串也走这里。存量数据理论上都是 video_published，
		// 但 status 列一直是 pending 的行没人清理过，不能假设它是干净的。
		return errUnknownEventType{msg.EventType}
	}
}

// pollOnce 捞一批 pending 行投出去。抽出来是为了让主循环的形状
// （while: 干活 → sleep）不被错误处理撑爆。
func pollOnce(ctx context.Context, db *gorm.DB, pubs OutboxPublishers) {
	var messages []video.OutboxMsg

	// Order("create_time ASC")：**按事件产生的顺序投递**。
	// 乱序投递不会造成数据错误（消费端 ZAdd 用 create_time 当 score，
	// 顺序无关），但会让排查时的日志时序变得没法看。
	// LIMIT 100 是防止一次捞太多把内存和 MQ 打爆 —— 积压了几万条时，
	// 一轮 100 条、每秒一轮，补完要几分钟，但**系统一直可用**。
	// 反过来一次全捞，就是自己给自己造了一次 DDoS。
	err := db.WithContext(ctx).
		Where("status = ?", "pending").
		Order("create_time ASC").
		Limit(100).
		Find(&messages).Error
	if err != nil {
		// 查询失败（比如 MySQL 闪断）→ 这一轮什么都不做，1 秒后重来。
		// 不退出、不重试到天亮：轮询器本身就是重试机制，不需要再套一层。
		log.Printf("[OutboxPoller] 查询 outbox 失败: %v", err)
		return
	}
	if len(messages) == 0 {
		return // 空表是常态。这里刻意不记日志 —— 每秒一条空日志会淹掉真日志。
	}

	log.Printf("[OutboxPoller] 捞出 %d 条 pending", len(messages))
	for i := range messages {
		msg := &messages[i]

		// 单条发布给一个自己的超时。5 秒足够一次 publish（实测 ≈0s），
		// 给到超时说明 broker 那边出问题了 —— 这时**必须放弃这一条**，
		// 否则一条卡住的消息会把整轮堵死，后面的行全都投不出去。
		pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := pubs.deliver(pubCtx, msg)
		cancel()

		if err != nil {
			// ---------- 毒消息要单独处理，否则它会永远堵在队首 ----------
			//
			// 未知的 EventType 和"投递失败"表面上看都是 err != nil，
			// 但**可恢复性完全相反**：
			//
			//	broker 抖动 / 目标 nil  → 可恢复。等下去就会好。**留着重投。**
			//	EventType 不认识        → 不可恢复。等一万年也不会变得认识。
			//
			// 不区分的话，这种行会永远占着 batch 里的一个位置：
			// 每轮 LIMIT 100 都会先捞出它，每秒刷一行日志，而它永远不会消失。
			// 积够 100 条这种行，轮询器就**彻底瘫痪**（每轮捞出来的全是它），
			// 而且是"日志一切正常、消息一条不发"的那种瘫。
			//
			// 所以标成 failed 让它离开 pending 集合。**不删行** ——
			// 删了就没人知道曾经有过这么一条；failed 留在表里，
			// 是一个能用一句 SQL 问出来的事实。
			var unknown errUnknownEventType
			if errors.As(err, &unknown) {
				log.Printf("[OutboxPoller] 毒消息（未知 EventType），标记 failed 后跳过: id=%d video=%d type=%q",
					msg.ID, msg.VideoID, msg.EventType)
				if uerr := db.WithContext(ctx).Model(&video.OutboxMsg{}).
					Where("id = ?", msg.ID).
					Update("status", "failed").Error; uerr != nil {
					log.Printf("[OutboxPoller] 标记毒消息失败（下轮还会捞到它）: id=%d err=%v", msg.ID, uerr)
				}
				continue
			}

			// **失败了什么都不做** —— 行留在表里，下一轮再来。
			// 这是整个 outbox 里最关键的一行：不删行 = 不丢消息。
			// 注意这里既不需要 Nack 也不需要重试计数，MySQL 的行本身就是队列。
			log.Printf("[OutboxPoller] 投递失败, 行保留待重投: id=%d video=%d type=%s err=%v",
				msg.ID, msg.VideoID, msg.EventType, err)
			continue
		}

		// 投递成功才删行。
		//
		// ⚠ 这里有一个**毫秒级的缝**，必须如实记下：
		// `basic.publish` 是**单向帧** —— 它没有回执。返回 nil 只代表
		// "消息写进了 socket"，**不代表 broker 收到了、更不代表进了队列**。
		// 所以存在这样一个窗口：publish 返回 nil → 我们删行 → 消息在网络上丢了。
		// 这时这条视频**永远不会进时间线**，而且任何地方都不会报错。
		//
		// 要堵住它，只能用 publisher confirm（把 channel 设成 confirm 模式，
		// 等 broker 的 ack 再删行）。本项目不做 —— 原项目也没做。
		// 代价是每条消息多一次往返，而收益是消灭一个极窄的窗口。
		// **知道缝在哪，别以为它是密的。**
		if err := db.WithContext(ctx).Delete(msg).Error; err != nil {
			// 删除失败只记日志：后果是这条消息**再投一遍**（消费端幂等，
			// 见下面 consumeTimeline 的 ZAdd —— 同 member 同 score 天然幂等）。
			// 重复投递比丢失轻得多，所以这里的处理方向和上面相反：
			// 上面是"宁可重投也不丢"，这里是"宁可重投也不留"。
			log.Printf("[OutboxPoller] 删除已投递的 outbox 行失败（下轮会重投）: id=%d err=%v", msg.ID, err)
		}
	}
}

// StartConsumer 启动 timeline 队列的消费者：把事件变成 Redis ZSet 里的成员。
//
// 它**跑在 API 进程**（理由见本文件开头）：写的是 Redis，而 Redis 连接
// 在 API 进程手里。性能上这也说得通 —— ZAdd 是 O(log N)，微秒级，
// 放在 API 进程里对请求延迟的影响可以忽略。
//
// 和 worker 进程里那几个消费者最大的结构差别：**外层多了断线重连**。
// worker 进程的重连在 runWorkerWithRetry 里（cmd/worker/main.go），
// 而这个函数是 API 进程里的一个 goroutine，没有那层壳，得自己写。
func StartConsumer(tmq *rabbitmq.TimelineMQ, queueName string, cache *rediscache.Client, rmq *rabbitmq.RabbitMQ) {
	if tmq == nil || cache == nil || rmq == nil {
		log.Printf("[TimelineConsumer] 依赖不全（tmq/cache/rmq 有 nil）, 消费者不启动（事件将积压在 %s）", queueName)
		return
	}

	go func() {
		for {
			if err := consumeTimeline(queueName, cache, rmq); err != nil {
				log.Printf("[TimelineConsumer] 消费中断, 5 秒后重连: %v", err)
			}
			time.Sleep(5 * time.Second)
		}
	}()
}

// consumeTimeline 一次连接的生命周期：开 channel → 声明消费 → 阻塞在消息循环里。
// 返回 error 就是"连接断了"，由外层重启。**永不主动返回**（除非断开）。
func consumeTimeline(queueName string, cache *rediscache.Client, rmq *rabbitmq.RabbitMQ) error {
	// 每次重连都**新建 channel**，不和发布者（TimelineMQ 那条）共用。
	// amqp.Channel 不是线程安全的，一条 channel 被发布者和消费者同时用，
	// 表现是帧交错 —— 随机某条消息 Ack 到了另一条上，属于最难查的一类 bug。
	ch, err := rmq.NewChannel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()

	// prefetch=10，比 worker 进程那边的 50 小得多。理由：
	// 这个消费者要写 Redis，而 Redis 是**共享资源**（请求路径也在用）。
	// 一次在手 50 条会形成一小波 ZAdd 脉冲，可能挤到请求路径的 Redis 调用。
	// 10 条足够吃满吞吐，又不会攒出脉冲。**prefetch 的大小应该按
	// "消费端持有资源多久"来定，不是越大越好。**
	if err := ch.Qos(10, 0, false); err != nil {
		log.Printf("[TimelineConsumer] 设置 Qos 失败（继续，无背压）: %v", err)
	}

	deliveries, err := ch.Consume(queueName, "", false /*手动Ack*/, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Printf("[TimelineConsumer] 开始消费 queue=%s", queueName)

	for d := range deliveries {
		evt, ok := decodeEvent[rabbitmq.TimelineEvent]("TimelineConsumer", d.Body)
		if !ok {
			// 毒消息：Ack 丢弃，别 Nack —— Nack+requeue 会让它无限循环回来。
			_ = d.Ack(false)
			continue
		}
		if evt.VideoID == 0 {
			_ = d.Ack(false)
			continue
		}

		// 500ms 超时：比 publish 那边的 5 秒紧得多，因为 Redis 是本机/内网
		// 服务，正常是亚毫秒级。给到超时说明 Redis 出问题了，这时**Nack 重入队**
		// 比"等下去"好 —— 消息回到队列，等 Redis 好了再处理。
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := cache.ZAdd(ctx, cache.Key(timelineKey), redis.Z{
			// Score = 视频创建时刻的毫秒数。**这是整个时间线的排序键。**
			// 用 create_time 而不是"投递时刻"，是为了让重投幂等 ——
			// 一条三小时前发布的视频被补投，它的排名仍然是三小时前的位置，
			// 不会突然插到最前面（TimelineMQ.PublishVideo 的注释里展开过）。
			Score:  float64(evt.CreateTime),
			Member: evt.VideoID,
		})
		cancel()

		if err != nil {
			log.Printf("[TimelineConsumer] ZAdd 失败, 消息重新入队: video=%d err=%v", evt.VideoID, err)
			// Nack(requeue=true) 把消息放回队列。这里**不能 Ack** ——
			// 时间线里少了这条视频是不会自愈的（不像缓存有 TTL 兜底）。
			_ = d.Nack(false, true)
			continue
		}

		// 裁剪到最新 1000 条。**失败仅记日志，仍然 Ack。**
		//
		// 为什么这里和上面区别对待：ZAdd 失败 → 数据缺失（不可自愈，必须重试）；
		// 裁剪失败 → ZSet 比你想要的大一点（功能完全正常，只是多占几 KB 内存）。
		// 为一个"大了一点"的后果去重试整条消息，等于用数据重复的风险
		// 去换一点内存 —— 不划算。
		//
		// ZRemRangeByRank(key, 0, -1001)：删掉排名 0 到倒数第 1001 名之间的，
		// 也就是**留下最后 1000 个**。（排名小 = score 小 = 时间早。）
		trimCtx, trimCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		if err := cache.ZRemRangeByRank(trimCtx, cache.Key(timelineKey), 0, -timelineMaxSize-1); err != nil {
			log.Printf("[TimelineConsumer] 裁剪时间线失败（不影响本次写入, 仅多占内存）: %v", err)
		}
		trimCancel()

		_ = d.Ack(false)
	}

	// for range 退出 = deliveries 被关闭 = channel 断了。
	// 这不是"消费完了"（队列是长活的），是**连接断了**，交给外层重连。
	return nil
}
