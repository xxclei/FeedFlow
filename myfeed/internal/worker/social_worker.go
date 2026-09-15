package worker

import (
	"context"
	"errors"
	"log"

	"myfeed/internal/middleware/rabbitmq"
	"myfeed/internal/social"

	"github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

// SocialWorker 消费 social.events 队列。
//
// 跑在 **worker 进程**（cmd/worker）。
//
// ---------- ⚠ 先说实话：这个 worker 是四个里最尴尬的一个 ----------
//
// 它在范式 B 下**几乎什么都不干**。这不是实现偷懒，是范式 B 的结构决定的，
// 必须讲清楚，否则以后看这段代码会以为它坏了。
//
// 回看 SocialService.Follow 的顺序：
//
//	① 校验 → ② repo.Follow() **同步写库** → ③ 失效缓存 → ④ 发 MQ
//
// 也就是说 MQ 消息到达这里时，**那条关注关系早就已经在库里了**。
// 于是下面 applyFollow 的 Insert **必然撞唯一索引 1062** —— 而且这不是异常，
// 这是**正常情况下唯一会走到的分支**。
//
// 所以这个文件真正的价值不是它的代码，是它记录的一件事：
//
//	**范式 B 里的"冗余双写"消费端，是一个空转的消费者。**
//
// 范式 A（like/comment）里 MQ 是唯一写路径，worker 是主角；
// 范式 B（social）里 MQ 只是"顺便喊一嗓子"，落库早就同步做完了。
// 把两种范式混在一个项目里，最容易出的错就是**照着 A 的样子写 B**——
// 于是写出一个"如果 1062 就表示出了问题"的判断，而实际上 1062 是常态，
// 这个 worker 会从上线第一天起就在误报。
//
// 那它为什么还要存在？两个理由，一个诚实一个不诚实：
//
//	诚实的：**队列必须有人消费**。不消费的话消息会在 social.events 里堆积，
//	       RabbitMQ 的内存和磁盘都吃紧（本项目设了 DLX，最终会进死信队列）。
//	       一个"没人消费的队列"是一个定时炸弹，不是一个无害的空操作。
//
//	不诚实的：原项目有它，所以照抄，保证拓扑完整、行为可对照。
//
// ---------- 它真正该干的活是什么 ----------
//
// 关注关系变化时，真正值得异步做的是 **feed 扇出**（把新关注的人的视频
// 塞进你的关注流）—— 那是阶段9 的 timeline/outbox 那一路，不是这里。
// 本 worker 保持最小：**把消息吃干净，不留下堆积**。
type SocialWorker struct {
	ch     *amqp.Channel
	social *social.SocialRepository
	queue  string
}

func NewSocialWorker(ch *amqp.Channel, socialRepo *social.SocialRepository, queue string) *SocialWorker {
	return &SocialWorker{ch: ch, social: socialRepo, queue: queue}
}

func (w *SocialWorker) Run(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return runConsumer(ctx, w.ch, "SocialWorker", w.queue, w.process)
}

// process 反序列化 + 校验 + 分发。
//
// ⚠ 注意它和 CommentWorker/LikeWorker 的一个结构差别：**这里永远返回 nil**
// 除了真正的 DB 故障。原因见上面那段 —— 范式 B 下"重复"是常态，
// 把它当错误重试的话，每条消息都会白重试 4 次（每次退避 1/2/4 秒）
// 才被丢弃，队列消费速度直接掉一个数量级。
//
// 这条纪律可以推广成一句：**幂等冲突不是错误，是"已经做过了"的另一种说法。**
// 判断标准是"重试能不能改变结果"—— 不能，就不该重试。
func (w *SocialWorker) process(ctx context.Context, body []byte) error {
	evt, ok := decodeEvent[rabbitmq.SocialEvent]("SocialWorker", body)
	if !ok {
		return nil
	}
	if evt.FollowerID == 0 || evt.VloggerID == 0 {
		return nil
	}
	// 自关注：MQ 封装那层已经拦过一次（socialMQ.go 的 publish），
	// 这里是第二道 —— 因为消息可能是别的生产者发进来的，不能假设它都守规矩。
	if evt.FollowerID == evt.VloggerID {
		return nil
	}

	switch evt.Action {
	case "follow":
		return w.applyFollow(ctx, evt.FollowerID, evt.VloggerID)
	case "unfollow":
		return w.applyUnfollow(ctx, evt.FollowerID, evt.VloggerID)
	default:
		return nil
	}
}

// applyFollow 插入关注关系。**1062 是预期结果**，见类型注释。
//
// 不复用 SocialRepository.Follow 的原因：它返回裸 error，不区分 1062
// （见 social/repo.go 里那段"原项目保留裸 500"的说明）。
// 接口层保留那个瑕疵是对的（用户重复点关注该拿到明确反馈），
// 但**异步层不行** —— 这里 1062 是常态，必须能和"真的写不进去"区分开。
// 所以这里直连 Create 并自己判 1062，而不是改 repo 的契约去迁就 worker。
//
// 这体现一个原则：**同一个动作在同步层和异步层需要的信息可以不一样，
// 不要为了让两边共用一段代码，把异步层需要的信息从同步层硬挤出来。**
func (w *SocialWorker) applyFollow(ctx context.Context, followerID, vloggerID uint) error {
	err := w.social.Follow(ctx, &social.Social{FollowerID: followerID, VloggerID: vloggerID})
	if err == nil {
		// 正常路径下**走不到这里**（同步写已经插过了）。走到了只有两种可能：
		//   ① 同步写失败了但消息还是发出去了（不该发生，但发生了也不坏）
		//   ② 有别的生产者往这个队列发关注事件
		// 两种情况下"插成功了"都是正确结果，记一条日志便于以后发现异常来源。
		log.Printf("[SocialWorker] 冗余写入真的插入了新行（同步路径本该已插）: follower=%d vlogger=%d",
			followerID, vloggerID)
		return nil
	}
	if isDuplicateKey(err) {
		// ← 正常情况下**每一条消息都走这里**。不是错误。
		return nil
	}
	return err // 真的写不进去（连接断了、表没了）才重试
}

// applyUnfollow 删除关注关系。删 0 行静默通过 —— 和 SocialService.Unfollow
// 注释里那条"接口层报错、异步层幂等"完全一致，这里不再重复。
//
// 不复用 SocialRepository.Unfollow 是因为它**没有返回 RowsAffected**
// （repo.go 顶上解释过：social 用实时 COUNT，不需要那个信息）。
// 这里同样不需要，所以直接用。
func (w *SocialWorker) applyUnfollow(ctx context.Context, followerID, vloggerID uint) error {
	return w.social.Unfollow(ctx, &social.Social{FollowerID: followerID, VloggerID: vloggerID})
}

// isDuplicateKey 判 MySQL 唯一键冲突（错误码 1062）。
//
// 和 video 包、search 包里的 isDupKey 是同一段逻辑的三个副本。
// **故意不抽成公共函数**，说明理由：
//
//	它依赖 go-sql-driver/mysql 这个具体的驱动类型，抽出去就得决定
//	"公共工具包"该放在哪 —— 而 internal/ 下没有一个合适的家（放 middleware
//	不搭，放 db 会让 db 包反向依赖驱动细节）。
//	更重要的是：**三行代码的重复比一个错误的抽象便宜得多。**
//	真要抽，正确的时机是出现第四处，且那时三段已经稳定不变。
//
// 和 like_repo.go 里那条注释同一条纪律：**比错误码，绝不比 err.Error() 的文案**。
// 文案会随驱动版本变（"Duplicate entry '1-2' for key ..."里的 key 名、
// 引号风格都改过），照着字符串判会静默失效 —— 失效的表现是 1062 被当成
// 未知错误疯狂重试，而不是报错，查起来极其难。
func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
