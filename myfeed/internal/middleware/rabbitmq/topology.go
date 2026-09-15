package rabbitmq

import amqp "github.com/rabbitmq/amqp091-go"

// 六条链路的**唯一权威清单**：交换机名 / 队列名 / 绑定 key。
//
// 为什么要单独抽一个清单，而不是让每个封装各自声明各自的：
//
// **worker 进程一个 MQ 封装都不建**（它只消费、从不发布），所以它没有
// `NewLikeMQ(base)` 这种构造函数可以顺带声明拓扑。它需要的是
// "把六条链路的名字一次性声明出来" —— 如果让它自己去 cmd/worker/main.go
// 里手抄这六组名字，就多了一份会漂移的副本：
// 哪天有人在 likeMQ.go 里把 `like.events` 打成了别的，worker 进程
// 声明的还是老名字，**两边不报错、不崩溃，只是消息去了两个不同的队列**。
//
// 所以这里做成单一来源，两边都从这里取。注意
// `topology_test.go` **故意不用**这份清单（它自己重抄一遍字面量）——
// 那不是重复劳动，那是刻意的第二意见：清单错了、代码错了，
// 测试按文档抄的第三份名字会红。
var businessLinks = []struct {
	Exchange   string
	Queue      string
	BindingKey string
}{
	{likeExchange, likeQueue, likeBindingKey},
	{commentExchange, commentQueue, commentBindingKey},
	{socialExchange, socialQueue, socialBindingKey},
	{popularityExchange, popularityQueue, popularityBindingKey},

	// ⚠ 这一条的 Queue 名和 Exchange 名**不一样**，是六条里唯一的例外
	// （见 timelineMQ.go 的说明）。因为这里取的是常量而不是手写的字符串，
	// 所以它不会像手抄那样被"顺手改齐"。
	{timelineExchange, timelineQueue, timelineBindingKey},

	// 转码（扩展，无阶段编号）。
	//
	// ⚠ 这条**和其余五条有一个本质区别**：它的消费者是**分钟级任务**。
	// 其余五条的消费都是毫秒级（落一行、ZAdd 一次、推一条 SSE），
	// 所以它们的 prefetch/Qos 可以从吞吐角度去调（50 都没问题）。
	// 转码不行 —— 一条消息占住的不是 prefetch 槽，是几十分钟的 CPU。
	// 消费端的 Qos(1) 和"不做优雅等待"都是从这里推出来的
	// （见 internal/worker/transcode_worker.go 开头）。
	{transcodeExchange, transcodeQueue, transcodeBindingKey},
}

// BusinessLinkCount 业务链路的条数。
//
// 存在的唯一理由是**让日志不至于撒谎**：cmd/worker 启动时会打一行
// "拓扑已声明（N 条链路）"，而那句话曾经是写死的字面量 ——
// 加转码那条链路时它没跟着改，于是日志说 5、实际声明了 6。
//
// 这种错误不会让任何功能失效，所以它**永远不会被测试发现**，
// 但它恰好是排查"拓扑是不是全建起来了"时唯一要看的那一行。
// 导出它比让 main 包再抄一份 len(businessLinks) 便宜得多 —— 后者是
// 第四处会漂移的副本（另外三处见下面那段关于队列名重复的说明）。
func BusinessLinkCount() int { return len(businessLinks) }

// DeclareAllTopology 一次声明全部 6 条链路的交换机、队列、绑定，以及
// 各自的死信队列（DeclareTopic 内部会带上）。
//
// **幂等**：RabbitMQ 的 declare 是"存在且参数一致就返回成功"。
// 所以 API 进程和 worker 进程各声明一遍完全没问题，谁先启动都行 ——
// 这正是它敢被两边同时调用的原因。参数不一致时会报
// PRECONDITION_FAILED，那是真出事了（比如有人给队列加了个参数却没删旧队列），
// 报错比静默好。
//
// 返回值故意是 error 而不是尽力而为：调用方（cmd/worker）需要知道
// "拓扑没建起来"，因为一个连不上队列的 worker 启动起来也没意义。
func DeclareAllTopology(ch *amqp.Channel) error {
	if ch == nil {
		return errChannelIsNil
	}
	for _, l := range businessLinks {
		if err := DeclareTopic(ch, l.Exchange, l.Queue, l.BindingKey); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 通知副本队列（阶段9）----------
//
// **刻意和 businessLinks 分成两份清单，不是一个列表里加三行**，
// 理由是它们的"归属"不同：
//
//	businessLinks        = 五条**业务**链路。API 发布、worker 进程消费。
//	                       两边都需要它们存在。
//	notificationLinks    = 三条**派生**链路。它们**复用业务链路的交换机**，
//	                       只是在自己这边多挂一条队列。**只有 API 进程需要。**
//
// 如果混成一份，cmd/worker 调 DeclareAllTopology 时就会顺手把三条通知队列
// 也声明了 —— 不报错，但表达了一个错误的事实："worker 进程依赖通知队列"。
// 依赖关系写错的地方，读代码的人会照着推理，然后推错。
//
// ---------- 它们为什么存在（Q8 的机械部分）----------
//
// 同一个交换机上的**不同队列 = 广播副本**，同队列 = 竞争消费。
// 通知要的是广播：点赞事件既要被 LikeWorker 拿去落库，也要被
// NotificationWorker 拿去发通知，**两件事都得发生**，不是二选一。
// 所以不能共用 like.events，必须另开一条队列绑到 like.events 这个交换机上。
//
// ---------- 只绑正向动作 ----------
//
// 三条的 binding key 是 `like.like` / `comment.publish` / `social.follow`，
// **都是精确匹配，没有通配符**。对比业务队列用的是 `like.*` / `comment.*` /
// `social.*` —— 差别就是"取关/删评论要不要通知"：
//
//	业务队列要 like.unlike（要落库，要把流水删掉）
//	通知队列不要 like.unlike（**取消点赞不该给人发通知，那是骚扰**）
//
// 这个"过滤"是在**交换机层**完成的，不靠消费端判断。好处是消费端
// 干净（进来的一定是关心的东西），代价是"为什么收不到"要多想一步 ——
// 所以两边都保留了 action 判断做双保险（见 notification_worker.go）。
var notificationLinks = []struct {
	Exchange   string
	Queue      string
	BindingKey string
}{
	{likeExchange, notificationLikeQueue, "like.like"},
	{commentExchange, notificationCommentQueue, "comment.publish"},
	{socialExchange, notificationSocialQueue, "social.follow"},
}

// 三个通知副本队列名。**必须和 cmd 侧消费时用的名字一致**（手写字面量，
// 和 likeQueueName 那边是同一个"跨包的刻意重复"，见 cmd/worker/main.go）。
const (
	notificationLikeQueue    = "notification.like"
	notificationCommentQueue = "notification.comment"
	notificationSocialQueue  = "notification.social"
)

// DeclareNotificationQueues 声明三条通知副本队列。**只有 API 进程调它**
// （见上面关于两份清单的说明）。
//
// 注意它**不声明交换机** —— 交换机由 DeclareAllTopology 建。
// 但这两个函数的调用顺序**不能反**：先建队列再建交换机的话，
// QueueBind 会因为交换机不存在而报 404 NOT_FOUND。
//
// 实际上 DeclareTopic 内部是"建交换机 → 建队列 → 绑定"的顺序，
// 而本函数用的是同一个 DeclareTopic（它建交换机的动作是幂等的），
// 所以**两个函数可以任意顺序调用**。这里记下这个设计的原因：
// 让每个声明函数自给自足（不依赖"别人已经建好了"），
// 比让调用方记住顺序要可靠 —— **顺序依赖是文档解决的事，不是代码解决的事，
// 而文档会过期。**
func DeclareNotificationQueues(ch *amqp.Channel) error {
	if ch == nil {
		return errChannelIsNil
	}
	for _, l := range notificationLinks {
		if err := DeclareTopic(ch, l.Exchange, l.Queue, l.BindingKey); err != nil {
			return err
		}
	}
	return nil
}
