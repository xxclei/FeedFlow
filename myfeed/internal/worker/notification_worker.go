package worker

import (
	"context"
	"log"
	"time"

	"myfeed/internal/middleware/rabbitmq"
	"myfeed/internal/notification"
	"myfeed/internal/video"

	amqp "github.com/rabbitmq/amqp091-go"
)

// NotificationHub 是"把通知推给在线用户"这件事的抽象。
//
// 存在的唯一理由是**解耦**：consumer 只应该知道"我要把这条通知送出去"，
// 不该知道"送出去"是往一个 SSE 长连接里写、还是往 WebSocket 里写、
// 还是发个推送。SSEHub 是它目前唯一的实现。
//
// 顺带解决了一个测试问题：没有这个接口的话，想单测 NotificationWorker
// 就得起一个真的 gin 服务器 + 一条真的 SSE 连接。有了它，传个
// 把通知塞进切片的假 hub 就能测。
//
// **签名里没有 error**，这是刻意的：推送是**尽力而为**的。
// 用户不在线 → Push 什么都不做 → 通知已经在表里了，他下次打开页面
// 走 ListHandler 拉得到。所以 Push 没有"失败"这个状态 ——
// 给它一个 error 返回值，只会诱使调用方去做无意义的重试。
type NotificationHub interface {
	Push(userID uint, n *notification.Notification)
}

// NotificationWorker 消费**通知副本队列**（notification.like / .comment / .social）。
//
// 跑在 **API 进程**（理由见 outboxworker.go 文件头：SSE 连接在 API 进程里，
// 跨不了进程）。这是三个一模一样的 worker，各接一条队列 ——
// 不是"三个不同的 worker"，所以本文件只有一份实现，构造三次。
//
// ---------- Q8：为什么不直接消费 like.events，非要另开副本队列？ ----------
//
// 因为**同队列是竞争消费**：notification.like 如果就是 like.events，
// 它和 LikeWorker 会互相抢消息，每条消息只有一个人拿到 ——
// 于是要么点赞落库了没通知，要么通知了没落库，**必然二缺一**。
//
// 解法是另开一条队列绑到**同一个 exchange** 上。topic 交换机的语义是
// "把消息复制给所有匹配的队列"，于是两条队列各拿一份完整副本、
// 各自 Ack、互不影响。这就是"广播副本"和"竞争消费"的分界：
//
//	同一个队列  → 竞争（一条消息给一个人）
//	不同的队列  → 广播（一条消息给每个队列一份）
//
// 决定这件事的是**队列**，不是交换机，也不是消费者。记住这一条，
// 以后看到"消息被抢走了"就能立刻定位到"两个消费者挂在了同一条队列上"。
//
// ---------- 为什么按 RoutingKey 分发，而不是三个 worker 三个文件 ----------
//
// 三种事件的动作完全同构（查收件人 → 写一行 notifications → 推送），
// 只是收件人和文案不同。拆成三个文件就是三份 95% 相同的代码，
// 而"同构的代码抄三份"正是 consumer.go 文件头明确反对的那件事。
type NotificationWorker struct {
	ch     *amqp.Channel
	videos *video.VideoRepository
	repo   *notification.NotificationRepository
	queue  string
	hub    NotificationHub
}

func NewNotificationWorker(
	ch *amqp.Channel,
	videos *video.VideoRepository,
	repo *notification.NotificationRepository,
	queue string,
	hub NotificationHub,
) *NotificationWorker {
	return &NotificationWorker{ch: ch, videos: videos, repo: repo, queue: queue, hub: hub}
}

func (w *NotificationWorker) Run(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return runConsumerWithDelivery(ctx, w.ch, "NotificationWorker("+w.queue+")", w.queue, w.process)
}

// StartNotificationConsumers 给每条通知队列起一个消费者，各自带**独立的
// channel 和独立的 5 秒重连循环**。
//
// ---------- 为什么复用 StartConsumer 的形状，而不是 runWorkerWithRetry ----------
//
// cmd/worker 里那个 runWorkerWithRetry 依赖 `base *rabbitmq.RabbitMQ` 和
// 一个**进程级 ctx**（由 signal.NotifyContext 提供），两者都是 worker 进程
// 才有的东西。API 进程里没有那个 ctx（gin 的启动是阻塞的，
// 这些 goroutine 就跟着进程一起生灭），所以这里用
// `context.Background()` —— 和 StartConsumer / StartOutboxPoller 一致。
//
// ---------- 为什么每个队列一条独立 channel ----------
//
// 老规矩：amqp.Channel 不是线程安全的。三条队列三个消费者 = 三个 goroutine，
// 共用一条 channel 会帧交错（随机某条消息 Ack 到了另一条上）。
// 更要紧的是**重连**：某一条队列的连接断了要重建，如果三条共用一条 channel，
// 重建就会把另外两条好好的消费者一起干掉。
// **独立的故障域要靠独立的资源来保证**，不是靠小心翼翼。
//
// ---------- rmq == nil 时整体不启动 ----------
//
// 这是文档 Q6 的降级姿势，值得原样保留：
//
//	通知链路**没有**"降级直写"这一说 —— 不像点赞（MQ 挂了就同步写 MySQL，
//	功能完全一样只是慢），通知不落库就等于不存在。
//	而"在 API 进程里同步写通知"会把通知的写入时间加到**点赞接口**的
//	响应时间里 —— 用一个次要功能去拖慢主流程，方向完全错了。
//
// 所以选择是：**安静地没有实时通知**。/notification/list 这些接口读的
// 是 DB，仍然能返回历史通知，只是不会再有新的。这个"部分可用"的边界
// 必须在启动日志里说清楚，否则运维会以为整个通知中心坏了。
func StartNotificationConsumers(
	rmq *rabbitmq.RabbitMQ,
	videos *video.VideoRepository,
	repo *notification.NotificationRepository,
	hub NotificationHub,
	queues []string,
) {
	if rmq == nil {
		log.Printf("[NotificationWorker] RabbitMQ 不可用, 通知子系统整体下线" +
			"（/notification/list 仍可读历史通知, 只是不会再有新的实时通知）")
		return
	}

	for _, q := range queues {
		queue := q // ← Go 1.22 之前闭包会捕获同一个变量，这个复制是必需的。
		// 本项目用的 Go 版本已经修了这个问题（每次迭代新变量），
		// 但**这行不是给编译器看的，是给读代码的人看的** ——
		// "这里的闭包捕获安全吗"不该是一个需要 reader 去查 Go 版本的问题。
		go func() {
			for {
				if err := consumeNotifications(rmq, videos, repo, hub, queue); err != nil {
					log.Printf("[NotificationWorker] %s 消费中断, 5 秒后重连: %v", queue, err)
				}
				time.Sleep(5 * time.Second)
			}
		}()
	}
}

// consumeNotifications 一次连接的生命周期。返回 error = 连接断了，外层重连。
func consumeNotifications(
	rmq *rabbitmq.RabbitMQ,
	videos *video.VideoRepository,
	repo *notification.NotificationRepository,
	hub NotificationHub,
	queue string,
) error {
	ch, err := rmq.NewChannel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()

	// prefetch=10：通知的写入是"一次 INSERT（可能 + 一次非阻塞推送）"，
	// 很轻。但**推送是拿 map 读锁的**，一次在手太多条会让推送形成脉冲，
	// 和 SSEHandler 的写 socket 抢时间。10 条足够。
	if err := ch.Qos(10, 0, false); err != nil {
		log.Printf("[NotificationWorker] 设置 Qos 失败（继续，无背压）: %v", err)
	}

	// 这里用 Background：API 进程里的后台消费者没有进程级 ctx，
	// 它们的生命周期就是这个进程（见上面 StartNotificationConsumers 的说明）。
	// 用 gin 的 ctx 会立刻踩到 outboxworker.go 里那个坑的姊妹版本。
	return NewNotificationWorker(ch, videos, repo, queue, hub).Run(context.Background())
}

// process 按 RoutingKey 分发。**这是全项目唯一一个用 routing key 而不是
// body 里的 Action 字段分发的消费者**，差别值得说清楚：
//
//	LikeWorker 们：一条队列收多种 action（like.like / like.unlike），
//	              队列是"一类业务的所有动作"，所以 action 在 body 里。
//	本 worker：   一条队列只收**一种** routing key（notification.like
//	              只绑 like.like），队列是"一种事件"，所以 action 在
//	              **绑定关系**里 —— body 里反而没有能够区分三种事件的公共字段。
//
// 这也解释了为什么这里的 default 分支是"Ack 丢弃"而不是报错：
// 收到意料之外的 routing key 说明**绑定配置和代码不一致**
// （比如有人把 notification.like 误绑到了 like.*）。重试不会让它变对。
func (w *NotificationWorker) process(ctx context.Context, d amqp.Delivery) error {
	switch d.RoutingKey {
	case "like.like":
		return w.handleLike(ctx, d.Body)
	case "comment.publish":
		return w.handleComment(ctx, d.Body)
	case "social.follow":
		return w.handleFollow(ctx, d.Body)
	default:
		// 只记日志不报错 —— 但**必须记**：静默吞掉未知 routing key 会让
		// "绑定配错了"这件事永远发现不了（表现只是"通知怎么不推了"）。
		log.Printf("[NotificationWorker] 未知 routing key, 丢弃: %q", d.RoutingKey)
		return nil
	}
}

// handleLike 点赞通知：收件人 = 视频作者，发件人 = 点赞的人。
//
// 两处"不发通知"的判断，都是**产品规则**不是技术防御：
//
//	authorID == 0   → 视频没了（或者查询失败），没有收件人，跳过
//	authorID == 点赞者 → **自己点赞自己的视频不通知自己**
//
// 第二条尤其重要：不判的话，用户给自己视频点个赞就会收到一条
// "你点赞了你的视频"，这是最容易被用户骂的那类 bug。
//
// 注意这里**没有走 IsExist 预检**，而是直接 GetByID 从视频上取 authorID ——
// 一次查询同时回答"视频在不在"和"作者是谁"，而 IsExist 只能回答前者。
// 这是"按需要的信息选查询"，不是"少查一次"的微优化。
func (w *NotificationWorker) handleLike(ctx context.Context, body []byte) error {
	evt, ok := decodeEvent[rabbitmq.LikeEvent]("NotificationWorker", body)
	if !ok {
		return nil
	}
	if evt.UserID == 0 || evt.VideoID == 0 {
		return nil
	}
	// 只通知正向动作。**取关/取消赞不通知** —— 那是骚扰，不是信息。
	// 注意这层判断在这里是**冗余的**：notification.like 队列绑的就是
	// `like.like`，like.unlike 的消息根本不会进来（在交换机层就分流掉了）。
	// 留着它是防御性的：绑定配置改了（变成 like.*）时，这行是第二道闸门。
	// **在交换机层挡掉 + 在消费端再挡一次**，比只靠一处可靠。
	if evt.Action != "like" {
		return nil
	}

	v, err := w.videos.GetByID(ctx, evt.VideoID)
	if err != nil {
		return err // 真故障（MySQL 挂了）→ 值得重试
	}
	if v == nil || v.AuthorID == 0 {
		return nil // 视频已被删除，这条通知没有意义
	}
	if v.AuthorID == evt.UserID {
		return nil // 自己赞自己，不通知
	}

	return w.save(ctx, &notification.Notification{
		RecipientID: v.AuthorID,
		SenderID:    evt.UserID,
		Type:        notification.TypeLike,
		// TargetID 存 **videoID** 而不是 like 的流水 id：前端点这条通知
		// 要跳回被赞的视频，所以这里放的是"动作对象"。
		// 三种通知的 TargetID 语义各不同，见 handleFollow 的对照说明。
		TargetID: evt.VideoID,
		Content:  "点赞了你的视频",
	})
}

// handleComment 评论通知：收件人 = 视频作者，发件人 = 评论的人。
//
// 和 handleLike 逐行同构，只有 Type 和文案不同 —— **这正是本文件
// 不拆成三个 worker 的理由**。两者的唯一真正差别在下面那条注释。
//
// 注意**自己评论自己的视频也不通知**（同一个判断）。评论区里
// 作者自己回复自己的视频是很常见的操作，通知自己会非常吵。
//
// 另有一处不一致照原项目保留：**评论正文里的 @提及走的是另一条路**
// （notifyMentions，同步写，Type="mention"），和这条 MQ 通知是两回事。
// 所以"评论了你的视频"和"在评论里提到了你"可能同时产生两条通知 ——
// 内容不同、Type 不同、前端渲染也不同，不是重复。
func (w *NotificationWorker) handleComment(ctx context.Context, body []byte) error {
	evt, ok := decodeEvent[rabbitmq.CommentEvent]("NotificationWorker", body)
	if !ok {
		return nil
	}
	// ⚠ comment.events 这条队列**同时收 publish 和 delete 两种事件**
	// （绑定 key 是 comment.*，见 topology.go），而 notification.comment
	// 只绑 comment.publish。所以这里同样有冗余的 action 判断 ——
	// 理由和 handleLike 里那段完全一样，不重复讲。
	if evt.Action != "publish" {
		return nil
	}
	if evt.AuthorID == 0 || evt.VideoID == 0 {
		return nil
	}

	v, err := w.videos.GetByID(ctx, evt.VideoID)
	if err != nil {
		return err
	}
	if v == nil || v.AuthorID == 0 {
		return nil
	}
	if v.AuthorID == evt.AuthorID {
		return nil // 自己评论自己的视频，不通知
	}

	return w.save(ctx, &notification.Notification{
		RecipientID: v.AuthorID,
		SenderID:    evt.AuthorID,
		Type:        notification.TypeComment,
		TargetID:    evt.VideoID,
		Content:     "评论了你的视频",
	})
}

// handleFollow 关注通知：收件人 = **被关注的人**，发件人 = 关注者。
//
// ---------- 它和上面两个的结构差别（值得对照着看） ----------
//
// 点赞/评论的收件人**不在事件里**，得查一次 videos 才知道是谁的视频被赞了。
// 关注的收件人**就在事件里** —— vlogger_id 就是被关注的人，直接可用。
//
// 这个差别源于事件本身的信息量：LikeEvent 说的是"谁赞了哪个视频"，
// 作者要回查；SocialEvent 说的是"谁关注了谁"，两个人都在里面了。
//
// 所以这里**一次数据库查询都不需要**（上面的两个各查一次 videos）。
// 这也解释了为什么 handleFollow 没有 ctx 之外的前置校验 ——
// 没有可查的东西，也就没有"查不到"的分支。
//
// ---------- TargetID 的语义对照 ----------
//
//	like    → TargetID = videoID  （点通知跳回被赞的视频）
//	comment → TargetID = videoID  （跳回被评论的视频）
//	follow  → TargetID = followerID（点通知跳去关注者的主页）
//
// 三种都是"动作对象"，但 follow 的动作对象是**人**不是物。
// 前端渲染时要按 Type 决定跳转路由 —— 拿 TargetID 直接当 videoID 用
// 会在 follow 通知上跳到一个不存在的视频页。
func (w *NotificationWorker) handleFollow(ctx context.Context, body []byte) error {
	evt, ok := decodeEvent[rabbitmq.SocialEvent]("NotificationWorker", body)
	if !ok {
		return nil
	}
	// 取关不通知（绑定层已经挡掉了，这里是第二道，理由同 handleLike）。
	if evt.Action != "follow" {
		return nil
	}
	if evt.FollowerID == 0 || evt.VloggerID == 0 {
		return nil
	}
	if evt.FollowerID == evt.VloggerID {
		return nil // 自关注，不通知（MQ 封装层也拦过）
	}

	return w.save(ctx, &notification.Notification{
		RecipientID: evt.VloggerID,
		SenderID:    evt.FollowerID,
		Type:        notification.TypeFollow,
		TargetID:    evt.FollowerID,
		Content:     "关注了你",
	})
}

// save 落库 + 推送，三个 handle 的公共尾巴。
//
// 顺序是**先落库再推送**，不能反：
//
//	① 落库成功、推送失败 → 用户当时没看到红点，但下次打开页面
//	   （ListHandler 读表）通知还在。**可恢复。**
//	② 推送成功、落库失败 → 用户看到了通知，但表里没有；
//	   刷新一下它**消失了**。这是最伤信任的一种表现。
//
// 所以"表是真相源，推送是锦上添花"这条顺序必须在每一处都保持一致。
//
// 推送**永远不返回错误**（NotificationHub 的签名里就没有 error），
// 所以这里也不会因为推送失败去重试整条消息 —— 重试会导致通知**落库两遍**，
// 而用户看到的是一条变两条。**宁可不推，也不重推。**
//
// 关于落库失败的重试：返回 error 会走 handleDelivery 的 4 次重试。
// 这里存在和 CommentWorker 同样的"重复插入"问题（通知表没有唯一索引，
// 同一条事件重投会插出两条一模一样的通知）—— 如实记下，不修，
// 理由和 CommentWorker 那边一样：去重表（EventID）的复杂度
// 换来的收益（少一条重复通知）不值。
func (w *NotificationWorker) save(ctx context.Context, n *notification.Notification) error {
	if err := w.repo.Create(ctx, n); err != nil {
		return err
	}
	if w.hub != nil {
		// 传 n 而不是它的副本：Create 已经回填了 n.ID，
		// 推送出去的那条 JSON 里带着 id，前端才能用它调 markRead。
		// 传副本的话 ID 是 0，前端点"已读"会打到一个不存在的 id 上。
		w.hub.Push(n.RecipientID, n)
	}
	return nil
}
