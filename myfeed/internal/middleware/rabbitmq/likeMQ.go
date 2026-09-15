package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// LikeMQ 是点赞链路的发布端。**本文件是 5 个链路封装的模板**，
// 其余 4 个（comment / social / popularity / timeline）结构一模一样，
// 只有常量、事件结构体的字段、入口方法的名字不同 —— 照着改即可。
//
// 每个封装**独占一条 channel**（见 rabbitmq.go 关于 Q9 的说明）：
// channel 不是线程安全的，5 条链路在 API 进程里并发发布，
// 共用一条就是帧交错。独占的成本只是 5 个 channel 对象，
// 而 broker 侧的 channel 配额上限是 2047，完全够用。
//
// ⚠ 注意：点赞发**两条**消息，而且是发到**两个不同的交换机**：
//
//	like.events              → 落 MySQL（likes 表 + 计数）
//	video.popularity.events  → 更新 Redis 热榜
//
// 第二条由 PopularityMQ 负责（popularityMQ.go），不在这个文件里。
// 之所以分开，是因为**目标、消费者、失败域都不同** ——
// MySQL 写失败和 Redis 写失败要能独立降级（见文档 Q2 和 Q3 的降级矩阵）。
type LikeMQ struct {
	ch *amqp.Channel
}

const (
	// 交换机名与队列名**同名**（原项目如此）。这是能用的但不是最佳实践：
	// 同名会让"交换机"和"队列"在管理台里长得一样，排查时容易点错。
	// 照抄是因为改名的收益（少一次看错）小于改名的成本（和原项目对不上号）。
	likeExchange = "like.events"
	likeQueue    = "like.events"

	// likeBindingKey 用通配 `like.*`：一条绑定同时接住 like.like 和 like.unlike，
	// 不用为每个动作单开队列。这是选 topic 交换机而不是 direct 的唯一理由。
	likeBindingKey = "like.*"

	// 两个 routing key。**它们是路由的依据** —— 消费端拿到消息后
	// 不看 action 字段也能从 RoutingKey 知道是什么动作。
	// （消费端两个都用了：RoutingKey 做粗判，action 字段做兜底。）
	likeLikeRK   = "like.like"
	likeUnlikeRK = "like.unlike"

	// action 字段的取值，同时也是 LikeEvent.Action 的合法值。
	likeActionLike   = "like"
	likeActionUnlike = "unlike"
)

// LikeEvent 是点赞/取消点赞的事件体。
//
// 字段名**必须和 09 文档的消息体一节逐字一致** —— 消费端（还有将来可能的
// 其他语言的消费者）按这些名字反序列化，改一个字就是线上静默失败。
//
// EventID 是 32 位 hex（16 字节随机），它的用途是**幂等的地基**：
// at-least-once 投递下同一条业务消息可能到达多次，消费端靠它认人。
// 本阶段消费端的幂等主要靠唯一索引（1062），EventID 是"将来要做去重表"
// 的挂载点 —— 但**从第一天就带上**，因为事后加字段意味着所有消费者
// 要一起改。这正是评论模块缺的那一块（评论没有唯一索引可撞）。
type LikeEvent struct {
	EventID    string    `json:"event_id"`
	Action     string    `json:"action"` // "like" / "unlike"
	UserID     uint      `json:"user_id"`
	VideoID    uint      `json:"video_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// NewLikeMQ 建封装：开一条独占 channel + 声明拓扑。
//
// **失败时的手工 Close 是这个构造函数唯一需要小心的地方。**
// 声明失败必须把 channel 关掉，否则它就一直挂在 broker 上 ——
// 每次重试都泄漏一条，直到撞上 2047 的配额上限，而且是"跑几天才炸"的那种。
//
// 这里**不能**用 `defer ch.Close()`：成功路径上 channel 要活下去，
// defer 会在函数返回的瞬间把它关掉，得到一个"构造成功但一用就报
// channel/connection is not open"的封装 —— 报错点和出错点隔了十万八千里。
// 所以只能手工在错误路径上关。
//
// base 为 nil 时会干净地失败（NewChannel 自己做了 nil 接收器保护），
// 这正是 API 进程在 MQ 连不上时"降级传 nil"路径需要的行为。
func NewLikeMQ(base *RabbitMQ) (*LikeMQ, error) {
	ch, err := base.NewChannel()
	if err != nil {
		return nil, err
	}
	if err := DeclareTopic(ch, likeExchange, likeQueue, likeBindingKey); err != nil {
		_ = ch.Close() // 见函数头：错误路径手工关，不能 defer
		return nil, err
	}
	return &LikeMQ{ch: ch}, nil
}

// Close 关掉这条独占 channel。
//
// nil 安全（和 rabbitmq.go 的 RabbitMQ.Close 同一套写法）：
// 降级态下 MQ 封装的指针可能是 nil，而退出流程是无条件 defer Close 的。
//
// **注意它只关 channel，不关 Connection** —— Connection 归基座所有，
// 由基座统一关闭。这里再关一次会把其他 4 条链路的 channel 一起打断。
func (l *LikeMQ) Close() error {
	if l == nil || l.ch == nil {
		return nil
	}
	return l.ch.Close()
}

// Like 发布一条"点赞"事件。
func (l *LikeMQ) Like(ctx context.Context, userID, videoID uint) error {
	return l.publish(ctx, likeActionLike, likeLikeRK, userID, videoID)
}

// Unlike 发布一条"取消点赞"事件。
//
// 它和 Like 唯一的差别是 action 和 routing key —— 但**消费端拿到的
// 两条消息长得几乎一样**，所以消费端必须认真区分（取消要点删流水、
// 计数 -1，把 unlike 当 like 处理就是灾难）。
func (l *LikeMQ) Unlike(ctx context.Context, userID, videoID uint) error {
	return l.publish(ctx, likeActionUnlike, likeUnlikeRK, userID, videoID)
}

// publish 是两个入口共用的实现。抽出来是因为 Like/Unlike 的差异
// **只有两个字符串**，复制一份的话将来加字段要改两处，迟早漏一处。
//
// 四步，顺序有讲究：
//
//	① 接收器/channel 判空 —— 降级态下这是常态，不是异常
//	② 参数校验 —— id 为 0 说明上游漏传了，发出去只会让消费端白跑一趟
//	③ 生成 EventID —— 放在校验之后，避免为一条注定发不出去的消息浪费随机数
//	④ PublishJSON
func (l *LikeMQ) publish(ctx context.Context, action, routingKey string, userID, videoID uint) error {
	if l == nil || l.ch == nil {
		return errors.New("like mq not initialized")
	}
	if userID == 0 || videoID == 0 {
		return errors.New("user_id and video_id are required")
	}

	eventID, err := newEventID(16)
	if err != nil {
		return err
	}

	return PublishJSON(ctx, l.ch, likeExchange, routingKey, LikeEvent{
		EventID:    eventID,
		Action:     action,
		UserID:     userID,
		VideoID:    videoID,
		OccurredAt: time.Now().UTC(),
	})
}

// ---------- 关于事件时间戳：本文件是 5 个封装的统一口径 ----------
//
// 文档记了原项目的一个小瑕疵：Like 的 OccurredAt 用 time.Now()，
// Comment/Social/Popularity 用 time.Now().UTC()，**两条链路的时间戳时区不一样**。
//
// 这不影响功能（JSON 里带时区偏移，两边都能解析回同一个瞬间），
// 但会让人看日志时怀疑人生：同一秒发生的两件事，一条写 "+08:00"、
// 一条写 "Z"，肉眼比对时要先在脑子里做一次换算。
//
// **本项目统一成 UTC**（文档也说这样更好），全 5 个封装一致。
// 判据很简单：时间戳的消费方是机器，机器只认绝对时刻；
// 而"对当地人是几点"是展示层的事，该在前端做，不该烙进事件体里。
