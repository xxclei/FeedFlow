package worker

import (
	"context"
	"time"

	"myfeed/internal/middleware/rabbitmq"
	"myfeed/internal/video"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

// CommentWorker 消费 comment.events 队列，把评论的发表/删除落进 MySQL。
//
// 跑在 **worker 进程**（cmd/worker）。它和 LikeWorker 是一对：
// 两个都是"范式 A"的落库端 —— MQ 是唯一写路径，请求侧只发消息不等结果。
//
// ---------- 和 LikeWorker 最值得对比的一处：事务 ----------
//
// LikeWorker 的三个动作**不在事务里**（原项目的写法，like_worker.go 里
// 已如实记下那道缝）。CommentWorker **用事务**，而且这不是我加戏 ——
// 它是在**忠实搬移**同步路径的语义：
//
//	阶段9 之前的 CommentService.Publish 里就是一个事务：
//	    IsExistTx → CreateCommentTx → ChangePopularityTx(+1)
//
// 异步化只是把这**整个事务**从请求线程挪到消费线程，一行语义都不该变。
// 如果 worker 里拆成三条独立 SQL，就等于"顺手把原子性去掉了"——
// 那是偷换，不是重构。所以下面 applyPublish 的形状和
// CommentService.publishMySQLDirect 是逐行对应的。
//
// （对比之下 LikeWorker 没有事务可搬：它的同步版就没有。
//
//	两边不一致的根源在同步版本，不在 worker。知道这层就够，不要去"统一"它。）
type CommentWorker struct {
	ch       *amqp.Channel
	comments *video.CommentRepository
	videos   *video.VideoRepository
	queue    string
}

func NewCommentWorker(ch *amqp.Channel, comments *video.CommentRepository, videos *video.VideoRepository, queue string) *CommentWorker {
	return &CommentWorker{ch: ch, comments: comments, videos: videos, queue: queue}
}

func (w *CommentWorker) Run(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return runConsumer(ctx, w.ch, "CommentWorker", w.queue, w.process)
}

// process 反序列化 + 校验 + 分发。返回 error 表示"值得重试"。
//
// ⚠ 本 worker 有一条**范式 A 的固有缺陷**，必须写在最显眼的地方：
//
// **它没有幂等闸门。**
//
// LikeWorker 有唯一索引 (video_id, account_id) 兜底 —— 重复投递被 1062 挡下，
// created=false 直接 return。评论**没有这道防线**（评论表故意没有唯一索引，
// 一人对同一视频本来就可以发多条）。于是：
//
//	同一条 publish 事件被投递两次 → **插出两条一模一样的评论**，
//	视频热度被 +2。
//
// 什么时候会重复投递：Ack 之前进程崩溃、网络抖动导致 broker 重发 ——
// 也就是 at-least-once 的定义本身。**这不是"理论上可能"，是必然会发生的**，
// 只是低频。
//
// 为什么没修：事件里**有 EventID**（CommentEvent.EventID），修法明确 ——
// 加一张 `processed_events(event_id PRIMARY KEY)` 去重表，在同一事务里
// `INSERT` 它，撞 1062 就说明处理过了、直接 return nil。代价是每个事件多一次
// 写 + 多一张表要清理。本项目**不做**（原项目也没有），原因：评论重复的
// 业务伤害远小于点赞/关注（用户看到两条一样的评论，不会造成计数性错误），
// 而复杂度是实打实的。**知道缝在哪，别以为它是密的。**
//
// 顺带说明 delete 方向**不需要**去重表：删两次，第二次 RowsAffected=0，
// 被下面的 applyDelete 静默挡下 —— 删除天然幂等，插入不是。
// 这正是"幂等性来自操作的性质，不来自代码的勤奋"。
func (w *CommentWorker) process(ctx context.Context, body []byte) error {
	evt, ok := decodeEvent[rabbitmq.CommentEvent]("CommentWorker", body)
	if !ok {
		return nil // 毒消息，别浪费 4 次重试
	}

	switch evt.Action {
	case "publish":
		if evt.VideoID == 0 || evt.AuthorID == 0 {
			return nil
		}
		return w.applyPublish(ctx, evt)
	case "delete":
		if evt.CommentID == 0 || evt.VideoID == 0 {
			return nil
		}
		return w.applyDelete(ctx, evt)
	default:
		return nil
	}
}

// applyPublish 插评论 + 视频热度 +1，**同生共死**（在一个事务里）。
//
// 形状和 CommentService.publishMySQLDirect 逐行对应，包括事务内那次
// IsExistTx：从"请求侧预检视频存在"到"worker 真的去插"之间，隔着一条队列，
// 时间窗比同步版**大得多**（正常 ~10ms，积压时可能是分钟级）。
// 窗口越大，师傅在这中间删掉视频的概率越高 —— 所以这次事务内复查
// 在异步路径下比同步路径下更必要，不是可省的一步。
func (w *CommentWorker) applyPublish(ctx context.Context, evt *rabbitmq.CommentEvent) error {
	return w.comments.Transaction(ctx, func(tx *gorm.DB) error {
		exist, err := w.videos.IsExistTx(tx, evt.VideoID)
		if err != nil {
			return err
		}
		if !exist {
			// 视频已被删除。消息是在它删除**之前**发的，属于迟到的合法消息 ——
			// 业务上已无效，但不算处理失败（返回 error 会白重试 4 次再丢弃）。
			return nil
		}

		if err := w.comments.CreateCommentTx(tx, &video.Comment{
			Username: evt.Username,
			VideoID:  evt.VideoID,
			AuthorID: evt.AuthorID,
			Content:  evt.Content,
			// CreatedAt 用**消费时刻**而不是事件里的 occurred_at，
			// 和同步直写路径的 autoCreateTime 口径一致。
			//
			// 如实记下这处偏差：MQ 路径下的评论时间会比用户点"发表"晚
			// 一次队列往返（正常 ~10ms，积压时是积压时长）。
			// 评论区是时间升序，所以极端积压时可能有两三条评论的先后
			// 和实际发表顺序不一致。不影响正确性，不修。
			CreatedAt: time.Now(),
		}); err != nil {
			return err
		}

		return w.videos.ChangePopularityTx(tx, evt.VideoID, +1)
	})
}

// applyDelete 删评论 + 视频热度 -1。
//
// 注意这里**没有权限校验** —— 不是漏了。安全判断只能在有身份的地方做
// （请求侧已经做完了），worker 拿到的消息里根本没有"谁在删"这个信息，
// 也没有请求上下文可依。这是"事件里要带够上下文"的另一面：
// **带够的是业务需要的上下文，不是身份上下文。**
//
// 关于 RowsAffected：CommentService.deleteMySQLDirect 里删 0 行会**报错**
// （"comment not found"），这里删 0 行**静默通过**。两处语义不同是刻意的，
// 和 SocialService.Unfollow 注释里那条区分同源：
//
//	接口层的错误是给用户看的（"你的操作没生效"），
//	异步层的幂等是给系统看的（重复投递是正常的，重试不该报错）。
//
// 但这里比 social 那边多一层考虑：**删 0 行时绝不能跟着 -1 热度**。
// 并发删同一条评论时，两个请求都通过了权限校验、都发了消息，
// worker 收到两条 delete —— 第一条删掉并 -1，第二条删 0 行。
// 如果这里不看 deleted 就直接 -1，热度会被扣两次。
// 同步路径靠事务里的 RowsAffected 检查挡住这件事（见 deleteMySQLDirect），
// 异步路径下那个检查**必须在这里重新做一遍** —— 因为闸门的位置跟着写路径走了。
func (w *CommentWorker) applyDelete(ctx context.Context, evt *rabbitmq.CommentEvent) error {
	return w.comments.Transaction(ctx, func(tx *gorm.DB) error {
		deleted, err := w.comments.DeleteCommentTx(tx, &video.Comment{ID: evt.CommentID})
		if err != nil {
			return err
		}
		if !deleted {
			return nil // 幂等闸门：已经删过了，热度不能再减
		}
		return w.videos.ChangePopularityTx(tx, evt.VideoID, -1)
	})
}
