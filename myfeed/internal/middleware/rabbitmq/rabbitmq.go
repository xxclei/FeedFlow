// Package rabbitmq 是 MQ 发布端的基座。
//
// 它只做三件事：连上 broker、按需开 channel、把 JSON 发出去。
// **业务语义一概没有** —— "点赞要发两条消息""关注先写库再发"这类规矩
// 属于各链路封装（likeMQ.go / socialMQ.go）和 service 层，
// 基座掺进去就变成了一坨谁也读不懂的东西。
//
// 命名对齐 internal/middleware/redis：同级的中间件包，一样是
// "薄封装 + 大量中文注释交代为什么"的写法。
package rabbitmq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/url"
	"strconv"
	"time"

	"myfeed/internal/config"

	amqp "github.com/rabbitmq/amqp091-go"
)

// errChannelIsNil 是包内共用的一条错误。
//
// 为什么抽成变量而不是各处 errors.New：`ch == nil` 在每个函数的入口都要查
// （channel 是"谁用谁开"的，传 nil 进来是完全可能的调用错误），
// 而 errors.New 每次调用返回的都是**不同的实例** —— 想用 errors.Is
// 在调用方区分"是 channel 没开"和"是 broker 拒了"，就区分不出来。
// 一条包级变量能让错误可比较，代价为零。
var errChannelIsNil = errors.New("channel is nil")

// RabbitMQ 只持有 Connection，**不持有 Channel**。
//
// 这是本阶段最容易写错的一个决定，理由在文档 Q9：
// amqp.Channel **不是线程安全的**，而 Connection 是。
// 如果基座里缓存一条共用 channel，那么 API 进程里五条链路
// （like/comment/social/popularity/timeline）会并发往同一条 channel 上写，
// 结果是帧交错、协议错乱、随机崩 —— 而且是**偶发**的，压测才出得来，
// 本地随手点几下永远复现不了。
//
// 所以职责切得死死的：
//
//	Connection → 基座独占（长连接，线程安全，全进程一条）
//	Channel    → 谁用谁开（短命，非线程安全，每个封装/每个 worker 各一条）
//
// 全项目没有任何一处跨 goroutine 共用 channel。
type RabbitMQ struct {
	Conn *amqp.Connection
}

// NewRabbitMQ 建连接。
//
// **它是 MQ 三件套里唯一"失败必须 fatal"的一个**（对比 Redis 的降级态）：
// Redis 挂了功能照跑只是慢，MQ 挂了整个异步体系就没有入口了。
// 但注意"fatal"的位置在调用方（cmd/main.go），不在这里 ——
// 这里只负责老老实实把 error 返上去。API 进程在 MQ 连不上时
// 不是直接退出，而是走降级（cache 那样传 nil），这是阶段9 的核心设计。
func NewRabbitMQ(cfg *config.RabbitMQConfig) (*RabbitMQ, error) {
	if cfg == nil {
		return nil, errors.New("rabbitmq config is nil")
	}

	// 用 url.URL 拼而不是 fmt.Sprintf("%s://%s:%s@...")：
	// 后者在密码含 @ : / ? # 时会拼出一个语法错误的 URL，
	// 而且报错信息只会说"dial tcp: 解析失败"，看不出是密码的问题。
	// url.UserPassword 会正确转义，且生成的格式正是 amqp://user:pass@host:port/
	//
	// Path 是 "/" —— 这就是 vhost。默认 vhost 的名字恰好是一个斜杠，
	// 所以它长得像个空路径，但**不能省略**：少了它 broker 会拒绝连接。
	u := url.URL{
		Scheme: "amqp",
		User:   url.UserPassword(cfg.Username, cfg.Password),
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:   "/",
	}

	conn, err := amqp.Dial(u.String())
	if err != nil {
		return nil, err
	}
	return &RabbitMQ{Conn: conn}, nil
}

// Close nil 安全 —— 和 redis.Client.Close 同一套写法。
//
// 为什么必须 nil 安全：API 进程持有的是 `*RabbitMQ`，MQ 没起来时它是 nil。
// 退出流程是无条件 defer rmq.Close() 的，不判空就会在"MQ 没连上"
// 这条本来就已经降级的路径上再 panic 一次 —— 故障路径上的二次故障。
func (r *RabbitMQ) Close() error {
	if r == nil || r.Conn == nil {
		return nil
	}
	return r.Conn.Close()
}

// NewChannel 开一条新 channel。**每个 MQ 封装、每个 worker 各调一次**。
//
// 调用方的纪律（见 Q9）：拿到 channel 后自己负责 Close，
// 而且**构造失败时必须先 Close 掉**（NewLikeMQ 里的 `defer` 有讲究，
// 见那个文件）。channel 泄漏不会立刻报错，只会慢慢占满 broker 侧的
// channel 配额（默认 2047），是那种"跑三天才炸"的 bug。
func (r *RabbitMQ) NewChannel() (*amqp.Channel, error) {
	if r == nil || r.Conn == nil {
		return nil, errors.New("rabbitmq not connected")
	}
	return r.Conn.Channel()
}

// DeclareTopic 声明一条"topic 交换机 + 队列 + 绑定"的三件套。
//
// topic 是本项目唯一用到的交换机类型，选它的理由是**通配绑定**：
// 队列用 `like.*` 就能一次接住 like.like 和 like.unlike，
// 不用为每个动作单开一个队列。
//
// 队列参数里那句 x-dead-letter-exchange 是**每个队列都要带**的：
// 消息被 Nack(requeue=false) 或过期时，broker 会把它投到 dlx.events。
// 本阶段没有任何触发点（见文档 Q6 的诚实回答），但基础设施一步到位 ——
// 队列一旦用错误的参数建好，改参数是要删队列重建的，
// 所以宁可先带着这个参数，也不能事后补。
//
// DeclareDLX 的失败**只记日志不返回**：DLX 是"插座"，不是主链路。
// 因为 DLX 声明失败而让业务队列建不起来，等于让一个备用件拖垮主系统。
func DeclareTopic(ch *amqp.Channel, exchange, queue, bindingKey string) error {
	if ch == nil {
		return errChannelIsNil
	}

	if err := ch.ExchangeDeclare(
		exchange,
		"topic", // 类型：topic（支持 like.* 这类通配绑定）
		true,    // durable：broker 重启后交换机还在
		false,   // autoDelete：没队列绑着也不要自己删 —— 阶段9 的副本队列
		//          是后声明的，中间存在"暂时没有队列绑定"的窗口，
		//          autoDelete=true 会让交换机在这个窗口里被删掉
		false, // internal：false = 允许客户端直接发布
		false, // noWait：false = 等 broker 确认，别自己骗自己
		nil,
	); err != nil {
		return err
	}

	if _, err := ch.QueueDeclare(
		queue,
		true,  // durable：和交换机一致，消息才会落盘
		false, // autoDelete：同上，别自己删
		false, // exclusive：false = 多个消费者可以接同一个队列（竞争消费）
		false, // noWait
		amqp.Table{
			"x-dead-letter-exchange": DLXExchange,
		},
	); err != nil {
		return err
	}

	if err := ch.QueueBind(
		queue,
		bindingKey,
		exchange,
		false,
		nil,
	); err != nil {
		return err
	}

	// DLX 是尽力的，失败不影响主链路（理由见函数头）
	if err := DeclareDLX(ch, queue); err != nil {
		log.Printf("[rabbitmq] 声明死信队列失败（不影响主链路）: queue=%s, err=%v", queue, err)
	}
	return nil
}

// PublishJSON 把任意结构体打成 JSON 发出去。
//
// 三个投递属性都是有讲究的，缺一个都会在"broker 重启"这个场景下暴露：
//
//	ContentType   "application/json" —— 消费端（和其他人）能认出这是什么，
//	              别让接收方靠猜。RabbitMQ 管理台里也直接可读。
//	DeliveryMode  Persistent —— **消息写磁盘**。不设的话消息只在内存里，
//	              broker 一重启，队列里的积压全没。而积压恰恰是本项目
//	              最重要的场景（MQ 挂着的时候消息就堆在队列里等）。
//	Timestamp     事件发生时刻，不是消费时刻。
//
// 注意最后一个参数用 PublishWithContext 而不是 Publish：
// 请求取消时要能及时收手，否则一个卡住的 broker 会把 handler goroutine
// 连同请求一起吊死。不过这里传的 ctx 有讲究 —— **outbox 轮询器
// 绝不能用请求 ctx**（它是后台任务，没有请求），见 worker/outboxworker.go。
func PublishJSON(ctx context.Context, ch *amqp.Channel, exchange, routingKey string, payload any) error {
	if ch == nil {
		return errChannelIsNil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return ch.PublishWithContext(
		ctx,
		exchange,
		routingKey,
		false, // mandatory：false = 路由不到队列就静默丢弃。
		//        设 true 能拿到"没人收"的通知，但要求调用方开 NotifyReturn
		//        协程去读，否则反而会阻塞 —— 本项目拓扑是声明时就绑好的，
		//        不存在"发出去没人接"的正常路径，所以用 false
		false, // immediate：RabbitMQ 3.x 已废弃该参数，恒传 false
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now(),
			Body:         body,
		},
	)
}

// newEventID 生成一个随机事件 ID（n 字节 → 2n 个 hex 字符）。
//
// 这是**幂等的地基**：at-least-once 投递下同一条业务消息可能到达多次，
// 消费端靠 EventID 认人。用 crypto/rand 而不是 math/rand ——
// 后者是可预测的，而事件 ID 是去重键，被别人猜到就能伪造重复事件。
//
// 实现直接抄 internal/middleware/redis/redis.go 的 randToken：
// 同一个项目里两处"生成随机 ID"长得不一样，读代码的人会以为有什么深意。
func newEventID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
