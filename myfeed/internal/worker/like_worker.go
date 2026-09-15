// Package worker 是**消费端**的大本营：MQ 消息在这里变成 MySQL 的行和 Redis 的数。
//
// ⚠ 包名会骗人，先把这件事说清楚：
//
//	internal/worker/like_worker.go   ← 跑在 **worker 进程**（cmd/worker）
//	internal/worker/outbox.go        ← 跑在 **API 进程**（cmd/main 里的 goroutine）
//
// 同一个包，两个进程。分包不是按"跑在哪"，而是按"是不是消费端"。
// 判断标准只有一条：**跟用户马上要看到的东西有关，就留在 API 进程；
// 纯后台落库的，扔给 worker 进程。**
//
// 所以别看见 worker 包就以为它一定在 worker 进程里跑 ——
// 去 cmd/main.go 和 cmd/worker/main.go 看谁调用了谁，那才是真相。
//
// 本文件（LikeWorker）和 popularity_worker.go 跑在 **worker 进程**。
package worker

import (
	"context"
	"time"

	"myfeed/internal/middleware/rabbitmq"
	"myfeed/internal/video"

	amqp "github.com/rabbitmq/amqp091-go"
)

// LikeWorker 消费 like.events 队列，把点赞/取消落进 MySQL。
//
// 它的三个依赖和 LikeService 是一套 repo —— 同一个包（video）里的同名方法。
// 差别在于：LikeService 用的是 ...Tx 版本（因为它活在事务里），
// LikeWorker 用的是非事务版本（它不在事务里，见下面 applyLike 的说明）。
type LikeWorker struct {
	ch     *amqp.Channel
	likes  *video.LikeRepository
	videos *video.VideoRepository
	queue  string
}

func NewLikeWorker(ch *amqp.Channel, likes *video.LikeRepository, videos *video.VideoRepository, queue string) *LikeWorker {
	return &LikeWorker{ch: ch, likes: likes, videos: videos, queue: queue}
}

// Run 开始消费，阻塞直到 ctx 取消或 channel 断开。
// 重试、Ack、退避的骨架全在 consumer.go 的 runConsumer 里，这里只交业务逻辑。
func (w *LikeWorker) Run(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return runConsumer(ctx, w.ch, "LikeWorker", w.queue, w.process)
}

// process 反序列化 + 校验 + 分发。
// 返回 error 表示"值得重试"，返回 nil 表示"处理完了，别再投"。
func (w *LikeWorker) process(ctx context.Context, body []byte) error {
	evt, ok := decodeEvent[rabbitmq.LikeEvent]("LikeWorker", body)
	if !ok {
		return nil // 毒消息，别浪费 4 次重试
	}
	if evt.UserID == 0 || evt.VideoID == 0 {
		// 字段缺了同理：重试不会让缺失的字段长出来。
		return nil
	}

	switch evt.Action {
	case "like":
		return w.applyLike(ctx, evt.UserID, evt.VideoID)
	case "unlike":
		return w.applyUnlike(ctx, evt.UserID, evt.VideoID)
	default:
		// 未知 action：可能是将来加了新动作、老 worker 还没升级。
		// 不算失败（返回 nil），但记一条日志 —— 静默吞掉未知动作
		// 会让"新功能上线但老 worker 没重启"这种事查起来毫无头绪。
		// （原项目这里也不返回错误，做法一致。）
		return nil
	}
}

// applyLike 真正干活的三个动作。
//
// ---------- 顺序不是随便排的 ----------
//
// **先做"会造成副作用"的动作，并且确认它真的发生了，再改计数。**
//
//	① IsExist        视频没了就整个跳过（业务上无效，不是错误）
//	② LikeIgnoreDuplicate  → created 这个 bool 就是**幂等闸门**
//	③ 只有 created==true 才动两个计数
//
// 为什么②必须在③前面：消息是 **at-least-once** 的 —— Ack 之前进程崩溃、
// 或者 outbox 中继"投递成功但删除失败"，同一条点赞事件都会再来一遍。
// 如果先改计数再插流水，第二次投递会把计数**再加一遍**，而流水表
// 因为有唯一索引只会有 1 行 —— 计数和真相从此对不上，且没有任何机制能纠正。
//
// 把流水插入的结果折叠成 created 这个 bool 再拿它当闸门，
// 重复投递就变成了"什么都不做"。**幂等不是"防止重复执行"，
// 是"重复执行的结果和不执行一样"。**
//
// ---------- 已知的缝（照抄原项目，这里如实记下）----------
//
// 三个动作**不在同一个事务里**（doc Q4 的写法就是如此）。
// 于是存在一个窗口：② 提交了、③ 执行到一半进程崩溃 →
// 流水有了但计数没加。重投时 created=false → 直接 return nil，
// **那个计数永远不会被补上**。
//
// 代价是"少算"而不是"多算"，方向和整个项目一致（欠比多好），
// 而且窗口窄。要堵住它，把三步包进一个 `repo.Transaction` 即可
// （那时插入要用事务版 tx.Create 判 1062）—— 属于文档"进阶改进"的范畴。
// **知道缝在哪，别以为它是密的。**
func (w *LikeWorker) applyLike(ctx context.Context, userID, videoID uint) error {
	ok, err := w.videos.IsExist(ctx, videoID)
	if err != nil {
		return err
	}
	if !ok {
		// 视频已被作者删除。事件是在它删除**之前**发出来的，属于
		// "迟到的合法消息" —— 业务上已经无效，但不算处理失败。
		// 这里返回 error 的话会白白重试 4 次，最后还是丢弃。
		return nil
	}

	created, err := w.likes.LikeIgnoreDuplicate(ctx, &video.Like{
		VideoID:   videoID,
		AccountID: userID,
		// CreatedAt 用**消费时刻**而不是事件里的 occurred_at，
		// 和 LikeService 直写路径的 time.Now() 口径一致。
		//
		// 这里有个小瑕疵，如实记下：MQ 路径下的 CreatedAt 会比
		// 直写路径晚几毫秒到几秒（取决于队列积压）。所以"我的点赞列表"
		// 按时间倒序时，走 MQ 的那几条位置可能和实际点击顺序有微小偏差。
		// 不影响正确性（排序键本来就只精确到秒级体感），不修。
		CreatedAt: time.Now(),
	})
	if err != nil {
		return err
	}
	if !created {
		// 幂等核心：流水早就有了，说明这条事件重复投递过。
		// **计数绝不能再加第二遍。**
		return nil
	}

	if err := w.videos.ChangeLikesCount(ctx, videoID, 1); err != nil {
		return err
	}
	return w.videos.ChangePopularity(ctx, videoID, 1)
}

// applyUnlike 和 applyLike 同构，方向相反。
//
// **注意这里没有 IsExist 预检** —— 不是漏了，是和 LikeService.Unlike
// 一样的不对称，理由也一样：取消点赞在"视频已被删"的情况下是无害的
// （删掉流水、两条计数 UPDATE 命中 0 行、结果自洽）。
// 而点赞在视频不存在时有害（会留下孤儿流水）。
// 不对称的根源是"插入"和"删除"对不存在的外键反应不同。
func (w *LikeWorker) applyUnlike(ctx context.Context, userID, videoID uint) error {
	deleted, err := w.likes.DeleteByVideoAndAccount(ctx, videoID, userID)
	if err != nil {
		return err
	}
	if !deleted {
		// 幂等闸门，和 applyLike 的 created 完全对称：
		// 删 0 行 = 流水早就没了（重复投递，或本来就没点过）。
		// **计数绝不能再减第二遍** —— GREATEST(x-1,0) 只防负数，
		// 防不了"凭空变少"，能防住它的只有这个 bool。
		return nil
	}

	if err := w.videos.ChangeLikesCount(ctx, videoID, -1); err != nil {
		return err
	}
	return w.videos.ChangePopularity(ctx, videoID, -1)
}
