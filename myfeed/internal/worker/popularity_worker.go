package worker

import (
	"context"
	"log"

	"myfeed/internal/middleware/rabbitmq"
	rediscache "myfeed/internal/middleware/redis"
	"myfeed/internal/video"

	amqp "github.com/rabbitmq/amqp091-go"
)

// PopularityWorker 消费 video.popularity.events 队列，把热度增量写进 Redis 分钟桶。
//
// ---------- 它和 LikeWorker 看起来很像，但有一个本质区别 ----------
//
// LikeWorker 的下游是 **MySQL**：MySQL 挂了要重试，因为数据不能丢。
// 这个 worker 的下游是 **Redis 热榜**：热榜本来就是**近似值** ——
// 桶的语义是"近 60 分钟的净互动量"，不是真相本身（真相在 MySQL 的
// popularity 列，由 LikeWorker 负责维护）。
//
// 所以它的 process **永远返回 nil**：UpdatePopularityCache 内部把错误
// 全吞了，一条消息处理完就直接 Ack，从不重试、从不进死信。
//
// 代价如实记下：**Redis 抖一下，那一次热度就永久丢了**，没有补偿入口
// （没有"重算"这回事，只能等桶过期自然归零）。这是有意接受的 ——
// 为了一个近似值引入重试 + 幂等 + 补偿，成本远大于收益。
//
// 这正是文档 Q2 说的"失败域不同"：正因为失败代价不同，
// 才要把热度拆成独立的一条链路，让"Redis 抖动"这种小事
// **连累不到"点赞落库"那条主线**。如果合成一条消息，
// 消费端就只能按最严格的那个标准来 —— Redis 一抖，落库也得跟着重试。
type PopularityWorker struct {
	ch    *amqp.Channel
	cache *rediscache.Client
	queue string
}

func NewPopularityWorker(ch *amqp.Channel, cache *rediscache.Client, queue string) *PopularityWorker {
	return &PopularityWorker{ch: ch, cache: cache, queue: queue}
}

func (w *PopularityWorker) Run(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return runConsumer(ctx, w.ch, "PopularityWorker", w.queue, w.process)
}

func (w *PopularityWorker) process(ctx context.Context, body []byte) error {
	evt, ok := decodeEvent[rabbitmq.PopularityEvent]("PopularityWorker", body)
	if !ok {
		return nil
	}
	if evt.VideoID == 0 || evt.Change == 0 {
		// Change == 0 是必须挡的，而且**发布端已经挡过一道**（PopularityMQ.Update）。
		// 这里再挡一次不是多余：消息可能来自别的生产者、或者是老版本发出的，
		// 而消费端执行 `ZINCRBY key member 0` 的代价是往桶里塞一个
		// score=0 的幽灵成员 —— ZINCRBY 会隐式创建 key 且不给 TTL，
		// 得靠桶过期才能消失。
		//
		// 一句话：**发布端的校验是体验，消费端的校验才是防线。**
		// （和全项目那句"预检只是体验、约束才是防线"是同一个道理，
		// 只不过这里的"约束"是消费端最后一道 if。）
		if evt.Change == 0 {
			log.Printf("[PopularityWorker] 收到 change=0 的事件, 丢弃: %+v", evt)
		}
		return nil
	}

	// 这个函数值不返回 error（错误全吞），所以下面的 return 恒为 nil ——
	// 消息永远不会重试。上面那段注释解释了为什么这是有意的。
	video.UpdatePopularityCache(ctx, w.cache, evt.VideoID, evt.Change)
	return nil
}
