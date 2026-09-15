package rabbitmq_test

// 五条链路拓扑的真机验收测试。
//
// 这个测试要证明的不是"代码能跑"，而是**broker 上真的长成了 09 文档
// 拓扑总表里那张表的样子**。拓扑错了代码照样编译通过、照样发布成功，
// 只是消息去了错的地方 —— 或者更糟，一条消息被两个消费者抢掉一半。
//
// 核心要验的一条（文档 Q8）：
//
//	同 exchange + 不同 queue = 广播副本（各拿一份）
//	同 queue                  = 竞争消费（只有一个拿到）
//
// 这是"点赞落库"和"点赞发通知"能同时发生的前提。搞错了就是二缺一，
// 而且不会有任何报错。
//
// 队列名/交换机名在这里**故意手写成字面量**（而不是引用包内的常量）：
// 常量是 unexported 的，外部测试包拿不到 —— 而这恰好是好事，
// 它逼着测试独立复述一遍文档里的名字。哪天有人在代码里把
// `like.events` 打成了 `like.event`，单测不会报、编译不会报，
// **但这里会红**。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"myfeed/internal/config"
	"myfeed/internal/middleware/rabbitmq"

	amqp "github.com/rabbitmq/amqp091-go"
)

// 文档拓扑总表里的五条业务链路（交换机名 = 队列名，timeline 除外）
var businessLinks = []struct {
	exchange string
	queue    string
}{
	{"like.events", "like.events"},
	{"comment.events", "comment.events"},
	{"social.events", "social.events"},
	{"video.popularity.events", "video.popularity.events"},
	{"video.timeline.events", "video.timeline.update.queue"}, // ← 唯一不同名的一条
}

// wipeTopology 把本测试会碰到的队列/交换机全删掉。
// 跑之前跑之后各一次：跑前清残留（否则参数不一致的残留会让声明报错，
// 而那个报错长得像"代码写错了"）。
func wipeTopology(t *testing.T, ch *amqp.Channel) {
	t.Helper()
	for _, l := range businessLinks {
		_, _ = ch.QueueDelete(l.queue, false, false, false)
		_, _ = ch.QueueDelete(l.queue+".dlx", false, false, false)
	}
	_, _ = ch.QueueDelete("notification.like", false, false, false)
	_, _ = ch.QueueDelete("notification.like.dlx", false, false, false)
	for _, l := range businessLinks {
		_ = ch.ExchangeDelete(l.exchange, false, false)
	}
	_ = ch.ExchangeDelete(rabbitmq.DLXExchange, false, false)
}

// setupTopology 一次性建出全部 5 个封装（每个封装自己开 channel + 声明拓扑）。
func setupTopology(t *testing.T) (*amqp.Channel, func()) {
	t.Helper()

	cfg, err := config.Load("../../../configs/config.yaml")
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	base, err := rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
	if err != nil {
		t.Skipf("连不上 RabbitMQ，跳过: %v", err)
	}

	// 先开一条"管理用" channel 做清理和查询
	adminCh, err := base.NewChannel()
	if err != nil {
		t.Fatalf("开管理 channel 失败: %v", err)
	}
	wipeTopology(t, adminCh)

	likeMQ, err := rabbitmq.NewLikeMQ(base)
	if err != nil {
		t.Fatalf("NewLikeMQ 失败: %v", err)
	}
	commentMQ, err := rabbitmq.NewCommentMQ(base)
	if err != nil {
		t.Fatalf("NewCommentMQ 失败: %v", err)
	}
	socialMQ, err := rabbitmq.NewSocialMQ(base)
	if err != nil {
		t.Fatalf("NewSocialMQ 失败: %v", err)
	}
	popularityMQ, err := rabbitmq.NewPopularityMQ(base)
	if err != nil {
		t.Fatalf("NewPopularityMQ 失败: %v", err)
	}
	timelineMQ, err := rabbitmq.NewTimelineMQ(base)
	if err != nil {
		t.Fatalf("NewTimelineMQ 失败: %v", err)
	}

	cleanup := func() {
		// 顺序：先关封装自己的 channel，再关管理 channel，
		// 最后清拓扑（清的时候还需要管理 channel）—— 所以清理必须在关闭之前
		wipeTopology(t, adminCh)
		_ = likeMQ.Close()
		_ = commentMQ.Close()
		_ = socialMQ.Close()
		_ = popularityMQ.Close()
		_ = timelineMQ.Close()
		_ = adminCh.Close()
		_ = base.Close()
	}
	t.Cleanup(cleanup)

	return adminCh, cleanup
}

// TestTopology_AllQueuesAndDLXExist 5 个封装建完，broker 上该有的都在。
func TestTopology_AllQueuesAndDLXExist(t *testing.T) {
	ch, _ := setupTopology(t)

	for _, l := range businessLinks {
		// 业务队列
		q, err := ch.QueueInspect(l.queue)
		if err != nil {
			t.Errorf("业务队列 %s 不存在: %v", l.queue, err)
			continue
		}
		// 死信队列（证明 DeclareTopic 内部的 DeclareDLX 被调到了）
		if _, err := ch.QueueInspect(l.queue + ".dlx"); err != nil {
			t.Errorf("死信队列 %s.dlx 不存在: %v", l.queue, err)
		}
		t.Logf("✅ 交换机 %-26s → 队列 %-28s 死信队列已建", l.exchange, q.Name)
	}
}

// TestTopology_BroadcastVsCompeting 本文件的核心测试。
//
// 场景：给 like.events 交换机**再挂一个副本队列**（notification.like），
// 模拟阶段9 通知链路的样子。然后发一条点赞事件，验证：
//
//	① 两个队列**各拿到一份**（广播 —— 这是通知和落库能同时发生的前提）
//	② 另一条链路的消息**不会**串进来（路由隔离）
func TestTopology_BroadcastVsCompeting(t *testing.T) {
	ch, _ := setupTopology(t)

	// 重开 LikeMQ：上面 setupTopology 里建的封装已经声明过 like.events，
	// 这里只是拿它来发布
	cfg, _ := config.Load("../../../configs/config.yaml")
	base, err := rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
	if err != nil {
		t.Fatalf("重连失败: %v", err)
	}
	defer base.Close()

	likeMQ, err := rabbitmq.NewLikeMQ(base)
	if err != nil {
		t.Fatalf("NewLikeMQ 失败: %v", err)
	}
	defer likeMQ.Close()

	popularityMQ, err := rabbitmq.NewPopularityMQ(base)
	if err != nil {
		t.Fatalf("NewPopularityMQ 失败: %v", err)
	}
	defer popularityMQ.Close()

	// ---- 挂通知副本队列：**同一个 exchange，不同的 queue** ----
	// 注意绑定 key 是 `like.like`（只有点赞通知，取消点赞不通知）
	if err := rabbitmq.DeclareTopic(ch, "like.events", "notification.like", "like.like"); err != nil {
		t.Fatalf("声明通知副本队列失败: %v", err)
	}
	t.Log("已挂载副本队列 notification.like（绑定 like.like）")

	ctx := context.Background()

	// ---- 发一条点赞事件 ----
	if err := likeMQ.Like(ctx, 1, 279); err != nil {
		t.Fatalf("Like 发布失败: %v", err)
	}
	// ---- 再发一条取消点赞事件（副本队列**不该**收到它）----
	if err := likeMQ.Unlike(ctx, 1, 279); err != nil {
		t.Fatalf("Unlike 发布失败: %v", err)
	}
	// ---- 发一条热度事件（like 队列**不该**收到它）----
	if err := popularityMQ.Update(ctx, 279, 1); err != nil {
		t.Fatalf("Update 发布失败: %v", err)
	}

	// 关键：等 broker 把上面三个 publish 帧处理完（见 settle 的说明）。
	// 少了这一行，下面的 drainCount 会稳定地取到 0 ——
	// 而且会伪装成"消息根本没发出去"，让人去怀疑完全正确的发布代码。
	settle()

	// ---- 验证 ① 广播：落库队列收到 2 条，通知队列收到 1 条 ----
	likeCount := drainCount(t, ch, "like.events")
	if likeCount != 2 {
		t.Errorf("like.events 队列 = %d 条，期望 2（like + unlike）", likeCount)
	}

	notifCount := drainCount(t, ch, "notification.like")
	if notifCount != 1 {
		t.Errorf("notification.like 队列 = %d 条，期望 **1** —— "+
			"绑定 key 是 like.like，取消点赞不该进通知队列", notifCount)
	}
	t.Logf("✅ 广播：like.events=%d 条，notification.like=%d 条（各拿一份，互不影响）", likeCount, notifCount)

	// ---- 验证 ② 路由隔离：热度队列 1 条，like 队列没多出来 ----
	popCount := drainCount(t, ch, "video.popularity.events")
	if popCount != 1 {
		t.Errorf("video.popularity.events 队列 = %d 条，期望 1", popCount)
	}
	// like.events 再查一次必须还是 0 —— 证明上一步的热度消息没串进来
	if extra := drainCount(t, ch, "like.events"); extra != 0 {
		t.Errorf("like.events 队列多出 %d 条 —— 路由串了", extra)
	}
	t.Logf("✅ 隔离：video.popularity.events=%d 条，没有串进 like.events", popCount)

	// ---- 验证 ③ 消息体字段名和文档逐字一致 ----
	// 重新发一条并保留下来检查 JSON
	if err := likeMQ.Like(ctx, 7, 300); err != nil {
		t.Fatalf("Like 发布失败: %v", err)
	}
	settle()
	d, ok, err := ch.Get("like.events", true)
	if err != nil || !ok {
		t.Fatalf("取消息失败: ok=%v err=%v", ok, err)
	}

	var raw map[string]any
	if err := json.Unmarshal(d.Body, &raw); err != nil {
		t.Fatalf("body 不是 JSON: %v", err)
	}
	t.Logf("LikeEvent 实际字段: %v", raw)

	for _, key := range []string{"event_id", "action", "user_id", "video_id", "occurred_at"} {
		if _, exists := raw[key]; !exists {
			t.Errorf("LikeEvent 缺字段 %q（消费端按这些名字反序列化，改一个字就是线上静默失败）", key)
		}
	}
	if got := raw["action"]; got != "like" {
		t.Errorf("action = %v，期望 \"like\"", got)
	}
	// event_id 是 16 字节 hex = 32 个字符
	if id, _ := raw["event_id"].(string); len(id) != 32 {
		t.Errorf("event_id 长度 = %d，期望 32（newEventID(16) → hex）", len(id))
	}
}

// TestTopology_TimelineQueueNameIsDifferent 单独盯住那条不同名的队列。
//
// 五条链路里只有 timeline 的队列名和交换机名不一样。这是最容易
// 被"想当然"改掉的一处（改代码的人会觉得"其他四个都同名，
// 这个肯定也是笔误"），所以给它一个专门的测试。
func TestTopology_TimelineQueueNameIsDifferent(t *testing.T) {
	ch, _ := setupTopology(t)

	if _, err := ch.QueueInspect("video.timeline.update.queue"); err != nil {
		t.Errorf("timeline 队列名不对，找不到 video.timeline.update.queue: %v", err)
	}
	// 反过来：**不该**存在一个叫 video.timeline.events 的队列
	// （那是交换机名，如果它也成了队列名，说明有人"顺手改齐了"）
	if _, err := ch.QueueInspect("video.timeline.events"); err == nil {
		t.Error("发现了名为 video.timeline.events 的队列 —— 队列名被改成和交换机同名了，" +
			"但文档要求的是 video.timeline.update.queue")
	}
	t.Log("✅ timeline 队列名 = video.timeline.update.queue（与交换机名不同，符合文档）")
}

// TestTopology_EventJSONShapes 其余 4 个事件的 JSON 形状。
func TestTopology_EventJSONShapes(t *testing.T) {
	ch, _ := setupTopology(t)

	cfg, _ := config.Load("../../../configs/config.yaml")
	base, err := rabbitmq.NewRabbitMQ(&cfg.RabbitMQ)
	if err != nil {
		t.Fatalf("重连失败: %v", err)
	}
	defer base.Close()

	commentMQ, _ := rabbitmq.NewCommentMQ(base)
	socialMQ, _ := rabbitmq.NewSocialMQ(base)
	timelineMQ, _ := rabbitmq.NewTimelineMQ(base)
	defer commentMQ.Close()
	defer socialMQ.Close()
	defer timelineMQ.Close()

	ctx := context.Background()

	t.Run("comment_publish", func(t *testing.T) {
		if err := commentMQ.Publish(ctx, "tom", 279, 1, "哈哈"); err != nil {
			t.Fatal(err)
		}
		settle()
		raw := getRaw(t, ch, "comment.events")
		t.Logf("CommentEvent(publish): %v", raw)
		for _, k := range []string{"event_id", "action", "username", "video_id", "author_id", "content", "occurred_at"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("缺字段 %q", k)
			}
		}
		// delete 专属字段不该出现（靠 omitempty）
		if _, ok := raw["comment_id"]; ok {
			t.Error("publish 事件里出现了 comment_id —— omitempty 没起作用")
		}
	})

	t.Run("comment_delete", func(t *testing.T) {
		// 注意第三个参数 videoID：这是本项目对原项目的**故意偏离**
		// （原项目 delete 事件不带 videoID，导致删评论时热度不回扣）
		if err := commentMQ.Delete(ctx, 42, 279); err != nil {
			t.Fatal(err)
		}
		settle()
		raw := getRaw(t, ch, "comment.events")
		t.Logf("CommentEvent(delete):  %v", raw)
		if _, ok := raw["comment_id"]; !ok {
			t.Error("delete 事件缺 comment_id")
		}
		if _, ok := raw["video_id"]; !ok {
			t.Error("delete 事件缺 video_id —— 热度 -1 会在 MQ 路径下静默丢失（本项目对原项目的修正）")
		}
		if _, ok := raw["content"]; ok {
			t.Error("delete 事件里出现了 content —— omitempty 没起作用")
		}
	})

	t.Run("social", func(t *testing.T) {
		if err := socialMQ.Follow(ctx, 1, 2); err != nil {
			t.Fatal(err)
		}
		settle()
		raw := getRaw(t, ch, "social.events")
		t.Logf("SocialEvent: %v", raw)
		for _, k := range []string{"event_id", "action", "follower_id", "vlogger_id", "occurred_at"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("缺字段 %q", k)
			}
		}
	})

	t.Run("timeline_create_time_is_unix_milli", func(t *testing.T) {
		createTime := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
		if err := timelineMQ.PublishVideo(ctx, 279, createTime); err != nil {
			t.Fatal(err)
		}
		settle()
		raw := getRaw(t, ch, "video.timeline.update.queue")
		t.Logf("TimelineEvent: %v", raw)

		got, ok := raw["create_time"].(float64) // JSON 数字解出来是 float64
		if !ok {
			t.Fatalf("create_time 不是数字: %T", raw["create_time"])
		}
		if int64(got) != createTime.UnixMilli() {
			t.Errorf("create_time = %d，期望 %d（UnixMilli）", int64(got), createTime.UnixMilli())
		}
	})

	t.Run("popularity_rejects_zero_change", func(t *testing.T) {
		popularityMQ, _ := rabbitmq.NewPopularityMQ(base)
		defer popularityMQ.Close()
		// change=0 必须被拦掉：否则消费端会执行一次 ZINCRBY 0，
		// 数值不变但**会创建一个 score=0 的幽灵成员**（ZINCRBY 隐式建 key）
		if err := popularityMQ.Update(ctx, 279, 0); err == nil {
			t.Error("change=0 竟然发布成功了 —— 会往热榜里塞幽灵成员")
		}
	})
}

// ---------- 一个必须知道的时序事实（写测试时踩出来的） ----------
//
// **`PublishJSON` 返回 nil，不代表 broker 已经收到消息。**
//
// 实测（.run 里跑过 10 轮）：
//
//	publish 调用耗时        ≈ 0s         ← 写进 TCP socket 就返回，不等确认
//	消息对另一个 channel 可见 ≈ 0.5 ~ 2ms  ← broker 才处理完那个帧
//
// 原因是 `basic.publish` 在 AMQP 里是**单向帧**：客户端发出去就不管了，
// 除非显式开启 publisher confirms（`ch.Confirm(false)`），
// 否则协议里根本没有"broker 收到了"这个回执。
//
// 两个后果：
//
//	① 测试里不能"发完立刻 Get" —— 会稳定地取到空，看起来像消息没发出去。
//	   必须等一拍。下面 settle() 就是干这个的。
//	② 生产代码里，`PublishJSON` 返回 nil **不等于**投递成功。
//	   理论上存在"publish 返回了、进程立刻崩溃、broker 其实没收到"的窗口
//	   （宽度约毫秒级）。对 like/comment/social 无所谓（它们有降级路径），
//	   但对 **outbox 那条链路**是个真实的缝：轮询器 publish 成功就删纸条，
//	   而"成功"其实只是"写进 socket 了"。
//	   真正堵住它的办法是给发布端开 publisher confirms（见文档"进阶改进"）。
//	   本项目照抄原项目，**没开** —— 记录下来，知道缝在哪。
const settleDuration = 200 * time.Millisecond

// settle 给 broker 时间处理异步的 publish 帧。
//
// 200ms 是实测可见延迟（2ms）的 100 倍余量，够到不能再够；
// 测试慢一点无所谓，**不稳定**才要命。
func settle() { time.Sleep(settleDuration) }

// drainCount 取空一个队列并返回取到多少条。
//
// **必须在 settle() 之后调用**（见上面的时序说明），否则会稳定地取到 0。
func drainCount(t *testing.T, ch *amqp.Channel, queue string) int {
	t.Helper()
	n := 0
	for {
		_, ok, err := ch.Get(queue, true)
		if err != nil {
			t.Fatalf("Get(%s) 失败: %v", queue, err)
		}
		if !ok {
			return n
		}
		n++
	}
}

// getRaw 取一条消息并解成 map（用于检查字段名）。
func getRaw(t *testing.T, ch *amqp.Channel, queue string) map[string]any {
	t.Helper()
	d, ok, err := ch.Get(queue, true)
	if err != nil {
		t.Fatalf("Get(%s) 失败: %v", queue, err)
	}
	if !ok {
		t.Fatalf("%s 队列里没有消息", queue)
	}
	var raw map[string]any
	if err := json.Unmarshal(d.Body, &raw); err != nil {
		t.Fatalf("body 不是 JSON: %v", err)
	}
	return raw
}
