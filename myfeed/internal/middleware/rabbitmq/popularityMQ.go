package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// PopularityMQ 是热度链路的发布端。结构同 likeMQ.go。
//
// 它和 LikeMQ 是**同一件事的两条腿**：一次点赞要发两条消息，
// 一条去落 MySQL（like.events），一条去更新 Redis 热榜（这里）。
//
// 为什么非要拆成两个交换机、两条消息，而不是发一条让消费者两头都做？
// 因为**失败域不同**（文档 Q2）：
//
//	MySQL 落库失败  → 点赞记录丢了，是最严重的问题，必须降级直写
//	Redis 热榜失败  → 榜单少一个数，MySQL 才是真相，记个日志就行
//
// 合成一条消息的话，消费端落库成功、写 Redis 失败，这条消息该不该重投？
// 重投 → 落库那段会被重复执行（靠幂等扛）；不重投 → Redis 就永远缺了。
// 两个目标的失败处理完全不同，**绑在一起就只能取最严格的那个**，
// 于是"Redis 抖动"这种小事会拖累"点赞落库"这条主线。
//
// 拆开之后各自的降级互不影响：这就是阶段8 那句
// "MySQL 必写 + Redis 尽力" 在 MQ 层的对应写法。
type PopularityMQ struct {
	ch *amqp.Channel
}

const (
	popularityExchange   = "video.popularity.events"
	popularityQueue      = "video.popularity.events"
	popularityBindingKey = "video.popularity.*"
	popularityUpdateRK   = "video.popularity.update"
)

// PopularityEvent 热度变更事件。
//
// **Change 是增量（±1），不是目标值。** 这一点必须和消费端的写法对上：
// Redis 侧是 `ZINCRBY`（原子累加），传增量才能让并发叠加。
// 如果这里传"目标值"，两个并发的点赞消息会互相覆盖，热度直接少算。
// （这正是阶段8 在同步路径上下过的结论，MQ 路径原样继承。）
//
// 注意**没有 Action 字段** —— 点赞(+1) 和取消(-1) 靠 Change 的正负区分，
// 不需要额外的动作字段。这和 LikeEvent 反过来：那边动作是"like/unlike"
// 两个语义不同的东西（要插/删一行），这边只是同一个数字的加减。
//
// ⚠ 这个事件**缺少补偿能力**，是它的已知短板：发布失败、消费失败、
// 解析失败，任何一种都无从补偿 —— 没有"重算"的入口，只能靠幂等
// 保证"不该多的不多"。接受它，因为热度本来就是近似值
// （Redis 桶是"近 60 分钟净增量"，不是真相本身，真相在 MySQL）。
type PopularityEvent struct {
	EventID    string    `json:"event_id"`
	VideoID    uint      `json:"video_id"`
	Change     int64     `json:"change"` // ±1
	OccurredAt time.Time `json:"occurred_at"`
}

func NewPopularityMQ(base *RabbitMQ) (*PopularityMQ, error) {
	ch, err := base.NewChannel()
	if err != nil {
		return nil, err
	}
	if err := DeclareTopic(ch, popularityExchange, popularityQueue, popularityBindingKey); err != nil {
		_ = ch.Close()
		return nil, err
	}
	return &PopularityMQ{ch: ch}, nil
}

func (p *PopularityMQ) Close() error {
	if p == nil || p.ch == nil {
		return nil
	}
	return p.ch.Close()
}

// Update 发布一条热度变更。change 传 +1（点赞/发评论）或 -1（取消/删评论）。
//
// **change == 0 必须拦掉**，这不是洁癖：一条 change=0 的消息发出去，
// 消费端会忠实地执行一次 `ZINCRBY key member 0` ——
// 数值上什么都不改，但它**会把一个原本不存在的 member 创建出来**。
// 阶段8 手工验 ZINCRBY 时确认过：ZINCRBY 会**隐式创建 key 且不给 TTL**。
// 所以一条"什么都没变"的消息，代价是往热榜桶里塞一个 score=0 的
// 幽灵成员，还得靠桶过期才能消失。
func (p *PopularityMQ) Update(ctx context.Context, videoID uint, change int64) error {
	if p == nil || p.ch == nil {
		return errors.New("popularity mq not initialized")
	}
	if videoID == 0 {
		return errors.New("video_id is required")
	}
	if change == 0 {
		return errors.New("change must not be zero")
	}

	eventID, err := newEventID(16)
	if err != nil {
		return err
	}

	return PublishJSON(ctx, p.ch, popularityExchange, popularityUpdateRK, PopularityEvent{
		EventID:    eventID,
		VideoID:    videoID,
		Change:     change,
		OccurredAt: time.Now().UTC(),
	})
}
