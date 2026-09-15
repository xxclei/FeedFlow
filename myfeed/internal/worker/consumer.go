package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// 本文件是**四个 worker 共用的消费骨架**。
//
// ---------- 一处对文档的刻意偏离 ----------
//
// 09 文档写的是"4 个 worker 同构，写完一个复制改" ——
// 也就是把这段重试循环在每个 worker 文件里各抄一份。
//
// 这里**不抄**，抽成一个函数。理由：
//
//	四个 worker 之间唯一的差别只有 process 那几行业务逻辑；
//	Try 次数、退避节奏、退出时 Nack 还是 Ack、deliveries 关闭怎么办 ——
//	**四个文件必须完全一致**，而且每一条都是踩过坑才写对的。
//	复制四份的结果是：哪天有人发现"退出时该用 Nack(requeue=true)"
//	或者改了退避上限，他得记得改四个地方 —— 而漏掉的那一个
//	不会报错、不会崩溃，只是在某个罕见路径上悄悄丢消息。
//
// 抽出来之后，"重试策略"成了单点，改一次全生效。
// 代价是 `NewLikeWorker(...).Run(ctx)` 这一层多绕了一次函数调用 ——
// 对比它省掉的漂移风险，这个价很划算。
//
// 复查时对照文档看业务逻辑即可：每个 worker 文件里剩下的只有 process。

// maxRetries 进程内重试次数。共试 maxRetries+1 = 4 次，退避 1s/2s/4s。
//
// 为什么是**进程内有界重试**而不是"Nack requeue 无限重试"：
// 无限重试遇到一条永远处理不完的毒消息，整条队列会被它卡死 ——
// 所有正常消息排在它后面永远轮不到。有界重试 + 最后 Ack 丢弃，
// 代价是极少数消息被丢，换来队列永远在流动。
//
// 这个取舍的前提是：**消息丢了不会造成不可挽回的后果**。
// 对 like/comment/social/popularity 成立（前三个有同步直写兜底，
// popularity 本来就是近似值）；对 outbox 那条链路就不成立 ——
// 所以 outbox 走的是另一套（消息表 + 轮询），
// 它的可靠性由"纸条删不删"保证，不靠 MQ 的重试。
const maxRetries = 3

// processFunc 是每个 worker 唯一需要提供的东西：
// 把消息体变成副作用。返回 error 表示"值得重试"，返回 nil 表示"处理完了，别再投"。
type processFunc func(ctx context.Context, body []byte) error

// deliveryFunc 是 processFunc 的"多要一点信息"版本：**整条 delivery**，
// 而不只是 Body。
//
// 只有 NotificationWorker 用它，原因是它要按 **RoutingKey** 分发
// （like.like / comment.publish / social.follow 三种事件共用一条逻辑，
// 光看 body 分不出来 —— 三种事件反序列化后字段名都不一样，
// 靠"哪个字段非零"猜是行不通的）。
//
// 为什么不让 processFunc 直接收 delivery、把四个老 worker 也改了：
// 那四个 worker **不需要** routing key，给它们多一个参数只会让
// "这个 worker 到底在乎什么"变得模糊。**参数表是接口意图的一部分** ——
// 一个只吃 Body 的函数在说"我不关心这条消息是怎么路由来的"，
// 这是有价值的信息，值得保留。
//
// 代价是两个几乎同构的入口函数。这个代价我付了，因为它换来的是
// 四个老 worker 一行不用改（也就没有任何引入回归的机会）。
type deliveryFunc func(ctx context.Context, d amqp.Delivery) error

// runConsumer 消费一个队列，阻塞到 ctx 取消或 channel 断开。
//
// **返回 error 是一种正常信号**，不是崩溃：外层 cmd/worker 的
// runWorkerWithRetry 看到 error 就关掉旧 channel、等 5 秒、重连。
// 所以不要把"连接断了"当致命错误处理，直接往上抛就是了。
func runConsumer(ctx context.Context, ch *amqp.Channel, name, queue string, process processFunc) error {
	// 把所有"只关心 Body"的 worker 适配到统一的 delivery 入口上。
	// 这一层适配就是上面那段"两个入口"的成本，一行。
	return runConsumerWithDelivery(ctx, ch, name, queue, func(ctx context.Context, d amqp.Delivery) error {
		return process(ctx, d.Body)
	})
}

// runConsumerWithDelivery 和 runConsumer 的区别只有一个：process 收整条 delivery。
func runConsumerWithDelivery(ctx context.Context, ch *amqp.Channel, name, queue string, process deliveryFunc) error {
	if ch == nil {
		return errors.New("consumer channel is nil")
	}

	// 三个 false 分别是：autoAck=false（**手动 Ack**，整个可靠性的地基）、
	// exclusive=false、noLocal=false。
	//
	// autoAck 必须为 false：autoAck=true 时 broker 在**投递出去的那一刻**
	// 就把消息删了，此时进程崩溃这条消息就永远消失 —— 那是 at-most-once，
	// 和我们要的 at-least-once 正好相反。
	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Printf("[%s] 开始消费 queue=%s", name, queue)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				// channel 断了。**不要在这里 sleep 重连** ——
				// 重连是外层 runWorkerWithRetry 的职责，这里只负责把
				// "我死了"这个事实报告上去。分层乱掉的话会出现
				// 两层各重连一次、channel 泄漏。
				return errors.New("deliveries channel closed")
			}
			handleDelivery(ctx, name, d, process)
		}
	}
}

// handleDelivery 单条消息的重试外壳。只管"试几次"，不管"业务对不对"。
//
// 这个分层让重试策略（次数、退避）和业务规则互不干扰，
// 改一个不会碰另一个 —— 也是它敢被四个 worker 共用的原因。
func handleDelivery(ctx context.Context, name string, d amqp.Delivery, process deliveryFunc) {
	for i := 0; i <= maxRetries; i++ {
		// 每一轮都先看进程是不是要退出了。
		//
		// 退出时用 Nack(requeue=true) 而**不是** Ack：消息放回队列，
		// 交给下一个消费者，而不是跟着这个正在关闭的进程一起陪葬。
		// 这是"进程重启不丢消息"的关键一行。
		select {
		case <-ctx.Done():
			_ = d.Nack(false, true)
			return
		default:
		}

		if err := process(ctx, d); err != nil {
			if i >= maxRetries {
				// 毒消息。**必须 Ack 丢弃**：不 Ack 的话这条消息会一直
				// 留在 unacked 状态，channel 一断就重投，然后再次失败 ——
				// 队列被它一个人堵死。
				log.Printf("[%s] 重试 %d 次后仍失败, 丢弃: %v", name, maxRetries, err)
				_ = d.Ack(false)
				return
			}
			// 指数退避 1s / 2s / 4s。退避而不是立刻重试，是因为
			// 失败大多是下游（MySQL）临时不可用 —— 立刻重试只会
			// 在同一个错误的时刻再撞一次。
			time.Sleep(time.Duration(1<<uint(i)) * time.Second)
			continue
		}

		_ = d.Ack(false)
		return
	}
}

// decodeEvent 是四个 worker 共用的"反序列化"前置步骤。
//
// ok=false 的语义是：**这条消息不值得重试**。
// 坏 JSON 重试一万次还是坏 JSON —— 所以直接让上层 Ack 丢弃，
// 而不是返回 error 让 handleDelivery 白白重试 4 次、浪费 7 秒退避。
//
// **字段校验故意留在各个 worker 里做**（不走这个泛型）：
// 每个事件的必需字段本来就不一样（LikeEvent 要 UserID+VideoID，
// PopularityEvent 要 VideoID+Change），硬塞进一个 validate 回调
// 只会让人在四个调用点猜"这个 bool 到底是 true 表示合法还是非法"。
// 泛型用在它真正统一的那一件事上：解 JSON。
func decodeEvent[T any](name string, body []byte) (*T, bool) {
	var evt T
	if err := json.Unmarshal(body, &evt); err != nil {
		log.Printf("[%s] 消息体不是合法 JSON, 丢弃: %v", name, err)
		return nil, false
	}
	return &evt, true
}
