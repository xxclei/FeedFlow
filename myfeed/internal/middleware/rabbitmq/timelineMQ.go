package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// TimelineMQ 是首页时间线链路的发布端。结构同 likeMQ.go，但有两处**独一份**：
//
//	① 队列名和交换机名**不一样**（其余四条都是同名）
//	② 它的调用方是 **outbox 轮询器**，不是 HTTP handler
//
// 第 ② 条带来一个全项目最隐蔽的坑，见文件末尾。
type TimelineMQ struct {
	ch *amqp.Channel
}

const (
	timelineExchange = "video.timeline.events"

	// ⚠ **队列名故意不叫 "video.timeline.events"**，这是五条链路里唯一的例外。
	// 其余四条都用了"队列名 = 交换机名"的原项目约定，这条不是。
	// 照抄即可（原项目如此），但要知道这里**没有隐含规则**——
	// 队列名除了排查时好看，跟交换机没有任何绑定关系，
	// 真正决定消息去哪的是 binding key。
	timelineQueue = "video.timeline.update.queue"

	timelineBindingKey = "video.timeline.*"
	timelinePublishRK  = "video.timeline.publish"
)

// TimelineEvent 视频发布时间线事件。
//
// CreateTime 是 **int64 毫秒时间戳（UnixMilli）**，不是 time.Time ——
// 这是全项目唯一一个用数字时间戳的事件字段，两个理由：
//
//	① 消费端的用途是 `ZADD feed:global_timeline <score> <video_id>`，
//	   而 **ZSet 的 score 必须是 float64**。直接给毫秒数字，消费端零转换。
//	② 时间线的排序**只认毫秒**，不需要时区信息。用 time.Time 反而
//	   要在两端各做一次 Parse/Format，多一个出错的地方。
//
// 而 OccurredAt（信封字段）仍然是 time.Time —— 它是给人看的排查线索，
// 不是参与计算的排序键。**两个时间字段语义不同，别合并。**
type TimelineEvent struct {
	EventID    string    `json:"event_id"`
	VideoID    uint      `json:"video_id"`
	CreateTime int64     `json:"create_time"` // UnixMilli，同时也是 ZSet score
	OccurredAt time.Time `json:"occurred_at"`
}

func NewTimelineMQ(base *RabbitMQ) (*TimelineMQ, error) {
	ch, err := base.NewChannel()
	if err != nil {
		return nil, err
	}
	if err := DeclareTopic(ch, timelineExchange, timelineQueue, timelineBindingKey); err != nil {
		_ = ch.Close()
		return nil, err
	}
	return &TimelineMQ{ch: ch}, nil
}

func (t *TimelineMQ) Close() error {
	if t == nil || t.ch == nil {
		return nil
	}
	return t.ch.Close()
}

// PublishVideo 发布"视频已发布"事件，让消费端把它塞进全局时间线。
//
// createTime 用 `video.CreateTime`，**不是 time.Now()**。
// 这两个在"发布"这个动作里通常只差几十毫秒，但语义完全不同：
// 时间线的排序键应该是"这条视频的创建时刻"，而不是"这条消息被投出的时刻"。
//
// 差别在**重投**时暴露：outbox 轮询器可能因为宕机/Redis 故障，
// 把一条几小时前的消息补投出去。用 time.Now() 的话，这条本该躺在
// 时间线中段的旧视频会**插到最前面**，看起来像是刚发布的。
// 用 video.CreateTime 则重投多少次，排名都不变 —— 这也是幂等的一部分。
func (t *TimelineMQ) PublishVideo(ctx context.Context, videoID uint, createTime time.Time) error {
	if t == nil || t.ch == nil {
		return errors.New("timeline mq not initialized")
	}
	if videoID == 0 {
		return errors.New("video_id is required")
	}

	eventID, err := newEventID(16)
	if err != nil {
		return err
	}

	return PublishJSON(ctx, t.ch, timelineExchange, timelinePublishRK, TimelineEvent{
		EventID:    eventID,
		VideoID:    videoID,
		CreateTime: createTime.UnixMilli(),
		OccurredAt: time.Now().UTC(),
	})
}

// ---------- 用这个封装时最容易踩的坑 ----------
//
// **绝不能用请求的 ctx。**
//
// 其余四个封装的调用方都活在 HTTP 请求里，ctx 从 gin 一路传下来，
// 请求结束就取消 —— 这是对的（客户端断开了，消息也就不必发了）。
//
// 但 TimelineMQ 的调用方是 **outbox 轮询器**：一个每秒跑一次的后台 goroutine，
// **它根本没有请求**。如果实现的时候顺手写了个 `ctx` 参数
// 并从某个请求域里传进来（或者更糟，传了个已经取消的 ctx），
// 后果是：轮询器第一次发布就返回 `context canceled`，
// 纸条一条都投不出去，而且**错误看起来像是 MQ 坏了**。
//
// 正确做法（在 worker/outboxworker.go 里）：
//
//	ctx := context.Background()                    // 或者带超时的版本
//	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
//
// 这也是文档"常见的坑"里点名的那一条：
// "outbox 轮询器里误用了请求 ctx → 请求结束轮询就死；它要用 context.Background()"。
//
// 对照阶段8 的 UpdatePopularityCache：那里用 `context.WithoutCancel(ctx)`
// 是**同一个问题的另一种解法** —— 保留 ctx 里的值但丢掉取消信号。
// 两条路径的形状不同（一个在请求里想活过请求，一个压根没有请求），
// 但精髓一致：**决定任务生死的不能是那个注定会结束的东西。**
