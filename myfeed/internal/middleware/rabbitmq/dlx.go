package rabbitmq

import (
	amqp "github.com/rabbitmq/amqp091-go"
)

// 死信交换机与重试上限。
//
// **诚实交代（文档 Q6）：本阶段 DLX 是个"装好了但没插电的插座"。**
//
// 基础设施是完整的一步到位：dlx.events 交换机、每个业务队列配套的
// <queue>.dlx 死信队列、GetRetryCount 读 x-death header 也都写好了。
// 但**当前没有任何 Nack(requeue=false) 的触发点**，所以消息进不了 DLX。
//
// 为什么明知用不上还要先建：
//
//	① 队列参数不可改。x-dead-letter-exchange 是**建队列时**的属性，
//	   事后再想要，只能删掉队列重建 —— 而那时候队列里可能正积压着
//	   几千条没消费的消息。所以宁可先带着。
//	② 它是"进阶改进"（见文档末尾）的挂载点：想接"重试 N 次后进死信
//	   人工排查"或者"延迟队列"，插座已经在了，改的是消费端的策略，
//	   不是基础设施。
//
// 消费端目前对毒消息的处理是**进程内有界重试**（4 次机会，退避 1s/2s/4s），
// 仍失败就 Ack 丢弃 —— 注意是 **Ack**，不是 Nack(requeue=false)。
// 这个选择在 worker 的 handleDelivery 里有详细解释：丢到 DLX 需要有人去看，
// 而当前项目没有人看，那还不如让它干净地消失，至少不会无限占着队列。
const (
	// DLXExchange 死信交换机。所有业务队列都把死信投到这里。
	DLXExchange = "dlx.events"

	// MaxRetryCount 允许的重试次数上限。
	//
	// 它的**当前用途只有一个**：给 GetRetryCount 一个比较基准，
	// 让消费端将来能在"已经死信过 3 次"时换个策略（比如直接丢弃、
	// 或者转人工）。本阶段消费端用的是自己循环里的 i（0..3），
	// 跟这个常量还没挂上钩 —— 这也是"插座已装、线还没接"的一部分。
	MaxRetryCount = 3
)

// DeclareDLX 为一条业务队列声明它的死信去处。
//
// 做两件事，而且**必须幂等**（每次建业务队列都会调一遍，包括每个 worker
// 每次重连之后）：
//
//	① 声明 dlx.events 交换机本身（topic，durable）
//	② 声明 <queueName>.dlx 队列并绑到 dlx.events 上，bindingKey 是 "#"
//
// 绑定用 "#" 而不是精确的 <queueName>：# 是 topic 里的"匹配零个或多个词"，
// 等于"所有死信都进这个队列"。理论上该按队列名精确绑定，但那要求
// 死信消息的 routingKey 恰好是原队列名 —— 而 RabbitMQ 的默认行为是
// **沿用原消息的 routingKey**（比如 like.like），绑 "#" 才是能收全的那个。
//
// 队列名加 .dlx 后缀（而不是建一个全局共用的死信队列）：让"哪条链路的
// 消息死的"在管理台里一眼可见。共用一个队列的话，排查时你得从
// x-death header 里一条条翻回去。
func DeclareDLX(ch *amqp.Channel, queueName string) error {
	if ch == nil {
		return errChannelIsNil
	}
	if queueName == "" {
		return nil // 没有队列名就没有死信去处，静默跳过（和上面 Err 不同：这是"没得做"，不是"做错了"）
	}

	if err := ch.ExchangeDeclare(
		DLXExchange,
		"topic",
		true,  // durable：死信要活得比 broker 长 —— 事后排查的前提是它还在
		false, // autoDelete
		false, // internal
		false, // noWait
		nil,
	); err != nil {
		return err
	}

	if _, err := ch.QueueDeclare(
		queueName+".dlx",
		true,  // durable
		false, // autoDelete
		false, // exclusive
		false, // noWait
		nil,   // **注意这里没有 x-dead-letter-exchange** —— 死信的死信没有去处，
		//       否则一个反复被拒的消息会在两队列之间来回弹，成了死循环
	); err != nil {
		return err
	}

	return ch.QueueBind(
		queueName+".dlx",
		"#", // 见函数头：收下所有死信
		DLXExchange,
		false,
		nil,
	)
}

// GetRetryCount 读出一条投递已经被死信过几次。
//
// RabbitMQ 把这段历史写在 x-death header 里，形状是：
//
//	"x-death": [ {"count": 2, "queue": "like.events", "reason": "rejected", ...} ]
//
// 是个**数组**（一条消息可能在不同队列里都死过），每项是一个
// amqp.Table（即 map[string]interface{}）。取第 0 项就是最近一次。
//
// 类型断言写成这样绕，是因为 AMQP 的字段类型由 broker 决定，
// count 被解出来可能是 int64 / int32 / int，取决于 amqp091-go 的版本
// 和消息来自哪个 broker 版本。**一个裸的 `.(int64)` 会在某次升级后
// 静默地恒返回 0**（类型断言失败不 panic，只是 ok=false）——
// 那会让"重试次数"永远显示 0，所有基于它的策略全部失效，而且不报错。
//
// 拿不到就返回 0（"没死过"是安全默认值：意味着消息会被当作全新的处理）。
func GetRetryCount(d amqp.Delivery) int {
	if d.Headers == nil {
		return 0
	}

	deaths, ok := d.Headers["x-death"].([]any)
	if !ok || len(deaths) == 0 {
		return 0
	}

	// 第 0 项 = 最近一次死信
	first, ok := deaths[0].(amqp.Table)
	if !ok {
		return 0
	}

	switch c := first["count"].(type) {
	case int64:
		return int(c)
	case int32:
		return int(c)
	case int:
		return c
	case float64: // JSON 往返后可能出现
		return int(c)
	}
	return 0
}
