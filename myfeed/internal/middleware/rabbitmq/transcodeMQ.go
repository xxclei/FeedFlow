package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// TranscodeMQ 转码链路的发布端。结构照 likeMQ.go 的模板，
// 但有三处**和其余五条链路都不同**，值得单独说：
//
//	① 它是唯一一条**消费者在 worker 进程、且任务耗时以分钟计**的链路
//	② 它是唯一一条**发布端是 outbox 轮询器**（而不是 HTTP handler）的链路
//	   —— 时间线那条也是轮询器发的，但转码这条还多了"发布时就要决定发不发"
//	③ 它的消息体是**六条里最小的**（只有 video_id），见下面 TranscodeEvent 的说明
//
// ① 的后果最重，落在消费端（internal/worker/transcode_worker.go）：
// Qos 必须是 1、不能做优雅等待、重跑前必须先清半成品目录。那三条的
// 完整推导写在消费端那个文件里，这里只记一句：**一条转码消息占住的
// 不只是 prefetch 槽，还有几十分钟的 CPU。**
type TranscodeMQ struct {
	ch *amqp.Channel
}

const (
	transcodeExchange = "video.transcode.events"
	transcodeQueue    = "video.transcode.queue"

	// Binding key 用通配：这条链路上现在只有一个动作（start），
	// 但将来大概率会有"取消转码""重新转码"—— 用通配就不用重开队列。
	// 这是选 topic 而不是 direct 的唯一理由，和 likeMQ 那边一致。
	transcodeBindingKey = "video.transcode.*"

	transcodeStartRK = "video.transcode.start"
)

// TranscodeEvent 转码任务事件。**故意只有 video_id。**
//
// ---------- 为什么不像别的链路那样把参数都塞进消息 ----------
//
// 直觉上应该把源文件的宽高、码率、要生成哪几档都带上 —— 那是发布时
// 已经算好的东西，让 worker 直接用，省一次 DB 查询。
//
// **这个直觉是错的**，理由是"消息是通知，数据库才是真相"：
//
//   - 消息会**重投**（at-least-once）。重投可能发生在几小时后 ——
//     那时源信息可能已经变了（重新发布、清理任务动过、手工改过）。
//     带上旧参数的副本会让 worker 按**过期的输入**去转码，而且不报错。
//   - worker 本来就必须读 DB：它要在**开始前**把 transcode_status 改成
//     running、**结束后**改成 ready/failed，还要写 hls_url。既然无论如何
//     都要读那一行，顺手把宽高一起读出来是多零成本的事。
//   - 消息体越小，积压时 broker 的内存占用越小，管理台里也更好读。
//
// 一句话：**能重算的都不要传，因为传过来的会过期，重算的不会。**
//
// EventID 仍然是 32 位 hex，用途和其余链路一致（幂等的地基）。
type TranscodeEvent struct {
	EventID    string    `json:"event_id"`
	VideoID    uint      `json:"video_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// NewTranscodeMQ 建封装：开一条独占 channel + 声明拓扑。
//
// 注意 **API 进程和 worker 进程都会声明这条拓扑**（API 走这里，
// worker 走 topology.go 的 DeclareAllTopology）—— 这是有意的重复，
// declare 是幂等的，谁先启动都行。
//
// 手工 Close 的写法同 likeMQ：错误路径上关，**不能 defer** ——
// defer 会在成功路径上也把它关掉，得到一个"构造成功但一用就报错"的封装。
func NewTranscodeMQ(base *RabbitMQ) (*TranscodeMQ, error) {
	ch, err := base.NewChannel()
	if err != nil {
		return nil, err
	}
	if err := DeclareTopic(ch, transcodeExchange, transcodeQueue, transcodeBindingKey); err != nil {
		_ = ch.Close()
		return nil, err
	}
	return &TranscodeMQ{ch: ch}, nil
}

func (t *TranscodeMQ) Close() error {
	if t == nil || t.ch == nil {
		return nil
	}
	return t.ch.Close()
}

// PublishTranscode 投一条转码任务。
//
// createTime 用 video.CreateTime 而不是 time.Now() —— 和 TimelineMQ.PublishVideo
// 同一个理由（重投时排序键不能变）。这里它**不参与排序**（转码队列是竞态的，
// 不保证顺序也不需要顺序），保留这个参数是为了让 outbox 的分流代码
// 能用同一套签名，不必为每条链路记住"这个参数这边要不要"。
func (t *TranscodeMQ) PublishTranscode(ctx context.Context, videoID uint, createTime time.Time) error {
	if t == nil || t.ch == nil {
		return errors.New("transcode mq not initialized")
	}
	if videoID == 0 {
		return errors.New("video_id is required")
	}
	_ = createTime // 见函数头：当前不参与计算，只为统一签名

	eventID, err := newEventID(16)
	if err != nil {
		return err
	}

	return PublishJSON(ctx, t.ch, transcodeExchange, transcodeStartRK, TranscodeEvent{
		EventID:    eventID,
		VideoID:    videoID,
		OccurredAt: time.Now().UTC(),
	})
}
