package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// SocialMQ 是关注链路的发布端。结构同 likeMQ.go。
//
// **但它的使用姿势和其他四个都不一样，这是本文件唯一值得细看的地方。**
//
// 关注走的是文档 Q3 里的"姿势二：先 DB 后 MQ"：
//
//	socialService.Follow:
//	  ① 同步写库（关注关系必须落库成功才算成功，用户要立刻看到）
//	  ② 然后才发 MQ
//	  ③ **MQ 发失败只记日志，照常返回 nil**
//
// 为什么它和 like/comment 不一样（那两条是"先试 MQ，失败才降级直写"）：
//
//   - 关注的**数据库写入是主业务本身**，不是副作用 ——
//     用户点"关注"要的就是"以后能刷到他的视频"，这件事必须当场确认。
//   - SocialWorker 只是**冗余消费者**：它落的那条 social 记录，
//     同步路径已经落过了（所以它必须靠 1062 幂等）。
//     它存在的意义是"万一同步路径没落成，还有一次补救机会"。
//   - 一个纯冗余的补救通道，丢一条无所谓 —— 为它做降级不值得：
//     降级路径能做的事，同步路径已经做完了。
//
// 所以这个封装的方法**调用方允许忽略返回的 error**（记日志即可），
// 而 LikeMQ.Like 的 error 是必须处理的（它决定要不要降级直写）。
// 同一个基座出来的两个封装，调用纪律不同 —— 别照抄错了。
type SocialMQ struct {
	ch *amqp.Channel
}

const (
	socialExchange   = "social.events"
	socialQueue      = "social.events"
	socialBindingKey = "social.*"
	socialFollowRK   = "social.follow"
	socialUnfollowRK = "social.unfollow"

	socialActionFollow   = "follow"
	socialActionUnfollow = "unfollow"
)

// SocialEvent 关注/取关事件。
//
// 字段名用 follower_id / vlogger_id，**不是** user_id / target_id ——
// 这是原项目的命名，而且它更好：关注是**有向关系**，
// "谁关注谁"这个方向性在字段名里一眼可见，比 user_id/target_id 少想一步。
//
// 顺带说明为什么取关也要发消息：
// **通知链路只绑正向动作**（`social.follow` 绑到 notification.social），
// 取关不通知（取关了还给人发通知，产品上是骚扰）。
// 但 `social.events` 队列绑的是 `social.*`，所以取关**会**进 SocialWorker ——
// 它要负责删掉那条 social 记录。
//
// 也就是说：**同一条消息，通知队列和落库队列看到的是不同的子集**。
// 靠的是两条队列的绑定 key 不同（`social.follow` vs `social.*`），
// 而不是靠消费端判断 —— 在交换机层就把不该来的挡掉，消费端更干净。
type SocialEvent struct {
	EventID    string    `json:"event_id"`
	Action     string    `json:"action"` // "follow" / "unfollow"
	FollowerID uint      `json:"follower_id"`
	VloggerID  uint      `json:"vlogger_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

func NewSocialMQ(base *RabbitMQ) (*SocialMQ, error) {
	ch, err := base.NewChannel()
	if err != nil {
		return nil, err
	}
	if err := DeclareTopic(ch, socialExchange, socialQueue, socialBindingKey); err != nil {
		_ = ch.Close()
		return nil, err
	}
	return &SocialMQ{ch: ch}, nil
}

func (s *SocialMQ) Close() error {
	if s == nil || s.ch == nil {
		return nil
	}
	return s.ch.Close()
}

// Follow 发布"关注"事件。调用方**允许忽略 error**（见类型注释）。
func (s *SocialMQ) Follow(ctx context.Context, followerID, vloggerID uint) error {
	return s.publish(ctx, socialActionFollow, socialFollowRK, followerID, vloggerID)
}

// Unfollow 发布"取关"事件。调用方同样允许忽略 error。
func (s *SocialMQ) Unfollow(ctx context.Context, followerID, vloggerID uint) error {
	return s.publish(ctx, socialActionUnfollow, socialUnfollowRK, followerID, vloggerID)
}

func (s *SocialMQ) publish(ctx context.Context, action, routingKey string, followerID, vloggerID uint) error {
	if s == nil || s.ch == nil {
		return errors.New("social mq not initialized")
	}
	if followerID == 0 || vloggerID == 0 {
		return errors.New("follower_id and vlogger_id are required")
	}
	// 自己关注自己：数据库那层有唯一索引拦着（follower+vlogger 的组合键），
	// 这里再加一道只是为了让消息别白跑一趟。**不加也不算错** ——
	// 但加了之后消费端就不必为这种畸形事件写分支。
	if followerID == vloggerID {
		return errors.New("cannot follow yourself")
	}

	eventID, err := newEventID(16)
	if err != nil {
		return err
	}

	return PublishJSON(ctx, s.ch, socialExchange, routingKey, SocialEvent{
		EventID:    eventID,
		Action:     action,
		FollowerID: followerID,
		VloggerID:  vloggerID,
		OccurredAt: time.Now().UTC(),
	})
}
