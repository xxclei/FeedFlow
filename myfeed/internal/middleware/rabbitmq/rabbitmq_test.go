package rabbitmq_test

// 基座的真机验收测试。
//
// 为什么基座值得一个测试：它是**全项目唯一"连不上就 fatal"的组件**
// （Redis 挂了降级、MySQL 挂了本来就没法跑、MQ 挂了整个异步体系没有入口）。
// 而它的大部分代码是"声明拓扑"—— 一段只在 broker 侧生效的副作用，
// 编译通过、go vet 通过、本地不跑都完全看不出来对错。
//
// 具体能验出来的错法：
//
//	x-dead-letter-exchange 漏了    → DLX 队列根本不会被建出来
//	DeliveryMode 没设 Persistent   → 消息不落盘，broker 重启就没了
//	绑定 key 写错                  → 消息发出去但队列收不到（静默丢）
//	Path("/") 漏了                 → 直接连不上
//
// 跑法（CWD 是包目录，所以配置往上退三级）：
//
//	cd myfeed && go test ./internal/middleware/rabbitmq/ -v
//
// **MQ 没起来时自动跳过**（t.Skip），这样 `go test ./...` 在没有 broker
// 的机器上不会红一片。跳过是有代价的（CI 里会静默不跑），
// 所以跳过的理由打印得很显眼。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"myfeed/internal/config"
	"myfeed/internal/middleware/rabbitmq"

	amqp "github.com/rabbitmq/amqp091-go"
)

// 测试专用名字。**故意不用 like.events 这些真名**：测试要删队列删交换机，
// 拿真名就等于在删生产拓扑。用独立命名空间，跑完清干净，互不打扰。
const (
	testExchange = "test.middleware.events"
	testQueue    = "test.middleware.events"
	testBinding  = "test.middleware.*"
	testRK       = "test.middleware.hello"
)

func setupMQ(t *testing.T) (*rabbitmq.RabbitMQ, *amqp.Channel) {
	t.Helper()

	cfg, err := config.Load("../../../configs/config.yaml")
	if err != nil {
		t.Fatalf("加载配置失败（CWD 必须是 internal/middleware/rabbitmq/）: %v", err)
	}

	base, err := rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
	if err != nil {
		// 跳过而不是失败：这个包的正确性不依赖"开发机上恰好开着 MQ"
		t.Skipf("连不上 RabbitMQ，跳过基座测试: %v\n"+
			"（想跑的话：docker start myfeed-rabbitmq）", err)
	}
	t.Cleanup(func() { _ = base.Close() })

	ch, err := base.NewChannel()
	if err != nil {
		t.Fatalf("开 channel 失败: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	return base, ch
}

// wipe 把测试用的队列/交换机删干净。跑之前跑之后各来一次 ——
// 跑之前是为了清掉上次失败留下的残留（否则 QueueDeclare 会因参数不一致而报错，
// 而那个报错长得像"代码写错了"，实际是"上次的垃圾没清"）。
func wipe(t *testing.T, ch *amqp.Channel) {
	t.Helper()
	// 第二个参数 ifUnused=false：队列里有消息也照删（测试残留里可能有）
	_, _ = ch.QueueDelete(testQueue, false, false, false)
	_, _ = ch.QueueDelete(testQueue+".dlx", false, false, false)
	_ = ch.ExchangeDelete(testExchange, false, false)
	_ = ch.ExchangeDelete(rabbitmq.DLXExchange, false, false)
}

// TestDeclareTopic_CreatesQueueAndDLX 验声明三件套真的在 broker 侧生效了。
func TestDeclareTopic_CreatesQueueAndDLX(t *testing.T) {
	_, ch := setupMQ(t)
	wipe(t, ch)
	t.Cleanup(func() { wipe(t, ch) })

	if err := rabbitmq.DeclareTopic(ch, testExchange, testQueue, testBinding); err != nil {
		t.Fatalf("DeclareTopic 失败: %v", err)
	}

	// ① 业务队列存在
	//    QueueInspect 拿不到会返回 channel 错误（404 NOT_FOUND）。
	//    注意 amqp.Queue 只有 Name/Messages/Consumers 三个字段 ——
	//    **durable 查不到**（它是 broker 侧的属性，不在 QueueInspect 的返回里）。
	//    所以"队列是 durable 的"这条只能靠管理台/HTTP API 确认，见下面的备注。
	q, err := ch.QueueInspect(testQueue)
	if err != nil {
		t.Fatalf("业务队列没建出来: %v", err)
	}
	t.Logf("业务队列: name=%s messages=%d consumers=%d", q.Name, q.Messages, q.Consumers)

	// ② 死信队列存在 —— 这一条证明 DeclareTopic 内部真的调到了 DeclareDLX，
	//    也就证明队列参数里的 x-dead-letter-exchange 起作用了
	//    （broker 不会为不存在的 DLX 建队列，但会接受这个参数并静默不投递；
	//     所以"DLX 队列存在"是能拿到的最强证据）
	if _, err := ch.QueueInspect(testQueue + ".dlx"); err != nil {
		t.Errorf("死信队列没建出来（x-dead-letter-exchange 没生效？）: %v", err)
	}

	// ③ 绑定生效：用 Passive 声明同一个队列 + binding 不该报错。
	//    （QueueBind 是幂等的，绑重复不会报错，所以这里只能证明"没崩"，
	//      真正的证明在下面的 PublishJSON 测试里 —— 收得到就说明绑对了）
	if err := ch.QueueBind(testQueue, testBinding, testExchange, false, nil); err != nil {
		t.Errorf("重新绑定失败: %v", err)
	}
}

// TestPublishJSON_RoundTrip 发一条真的，再从队列里取出来，验证三件事：
// 消息到了、body 能解回原值、投递属性是对的。
func TestPublishJSON_RoundTrip(t *testing.T) {
	_, ch := setupMQ(t)
	wipe(t, ch)
	t.Cleanup(func() { wipe(t, ch) })

	if err := rabbitmq.DeclareTopic(ch, testExchange, testQueue, testBinding); err != nil {
		t.Fatalf("DeclareTopic 失败: %v", err)
	}

	type payload struct {
		EventID string `json:"event_id"`
		UserID  uint   `json:"user_id"`
		VideoID uint   `json:"video_id"`
	}
	sent := payload{EventID: "deadbeef", UserID: 1, VideoID: 279}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rabbitmq.PublishJSON(ctx, ch, testExchange, testRK, sent); err != nil {
		t.Fatalf("PublishJSON 失败: %v", err)
	}

	// 从队列里主动取一条（autoAck=true：测试不想留 unacked 拖住队列删除）
	d, ok, err := ch.Get(testQueue, true)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if !ok {
		t.Fatal("队列里没有消息 —— 发布成功但没投递到队列，多半是绑定 key 写错了")
	}

	t.Logf("收到消息: contentType=%s deliveryMode=%d routingKey=%s body=%s",
		d.ContentType, d.DeliveryMode, d.RoutingKey, string(d.Body))

	// ① 投递属性 —— 这两个是"broker 重启后消息还在不在"的关键
	if d.ContentType != "application/json" {
		t.Errorf("ContentType = %q，期望 application/json", d.ContentType)
	}
	if d.DeliveryMode != amqp.Persistent {
		t.Errorf("DeliveryMode = %d，期望 %d(Persistent) —— 消息不会落盘，broker 重启就丢",
			d.DeliveryMode, amqp.Persistent)
	}
	if d.RoutingKey != testRK {
		t.Errorf("RoutingKey = %q，期望 %q", d.RoutingKey, testRK)
	}

	// ② body 能解回原值
	var got payload
	if err := json.Unmarshal(d.Body, &got); err != nil {
		t.Fatalf("body 不是合法 JSON: %v", err)
	}
	if got != sent {
		t.Errorf("往返后 = %+v，期望 %+v", got, sent)
	}

	// ③ 全新消息没有 x-death header → GetRetryCount 必须返回 0。
	//    这里顺带覆盖了"headers 为 nil"这条最常见的分支。
	if n := rabbitmq.GetRetryCount(d); n != 0 {
		t.Errorf("GetRetryCount = %d，期望 0（这条消息没死信过）", n)
	}
}

// TestNewRabbitMQ_BadCredentials 验证连不上时**返回 error 而不是 panic**。
//
// 这条看着琐碎，但它守着项目的降级设计：API 进程要在 MQ 连不上时
// 继续跑（把 nil 往下传），所以 NewRabbitMQ 必须是"干净地失败"。
func TestNewRabbitMQ_BadCredentials(t *testing.T) {
	cfg, err := config.Load("../../../configs/config.yaml")
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	bad := cfg.RabbitMQ
	bad.Password = "definitely-not-the-password"

	base, err := rabbitmq.NewRabbitMQ(&bad)
	if err == nil {
		_ = base.Close()
		t.Fatal("密码错误却连上了 —— 要么 broker 没开鉴权，要么凭据被忽略")
	}
	t.Logf("按预期失败: %v", err)

	// nil 接收器的安全性：降级态下调用方会无条件 Close
	var nilBase *rabbitmq.RabbitMQ
	if err := nilBase.Close(); err != nil {
		t.Errorf("nil 接收器 Close 应该返回 nil，却得到: %v", err)
	}
	if _, err := nilBase.NewChannel(); err == nil {
		t.Error("nil 接收器 NewChannel 应该返回 error")
	}
}

// TestNewRabbitMQ_NilConfig 配置为 nil 时也要干净地失败。
func TestNewRabbitMQ_NilConfig(t *testing.T) {
	if _, err := rabbitmq.NewRabbitMQ(nil); err == nil {
		t.Error("cfg 为 nil 却成功了")
	}
}
