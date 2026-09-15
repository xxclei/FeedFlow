package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// CommentMQ 是评论链路的发布端。结构照抄 likeMQ.go，只列不同点。
//
// 和 LikeMQ 最大的结构差异：**它有两个语义完全不同的入口**
// （发表 / 删除），事件体的字段也因此分成两套（publish 带内容，
// delete 只带 id）。所以这里不能用 LikeMQ 那种"两个入口共用一个
// publish，只换两个字符串"的写法 —— 得有两个独立的构造逻辑。
type CommentMQ struct {
	ch *amqp.Channel
}

const (
	commentExchange   = "comment.events"
	commentQueue      = "comment.events"
	commentBindingKey = "comment.*"
	commentPublishRK  = "comment.publish"
	commentDeleteRK   = "comment.delete"

	commentActionPublish = "publish"
	commentActionDelete  = "delete"
)

// CommentEvent 同时承载 publish 和 delete 两种事件。
//
// **字段全加了 omitempty，这不是随手加的**：两种事件的字段集不同，
// 不加 omitempty 的话 delete 事件会序列化成
// `{"username":"","video_id":0,"author_id":0,"content":"","comment_id":5}` ——
// 一堆零值字段混在真数据里。消费端如果按 `video_id == 0` 做判断，
// 就会在"字段不存在"和"字段存在但是 0"之间分不清，而这两件事语义完全不同。
//
// ⚠ **VideoID 在 delete 事件里是本项目对原项目的偏离，而且是必须的偏离。**
//
// 原项目的 delete 事件只有 comment_id。它的消费端拿到消息后
// **没有回查评论**，所以它根本不知道这条评论属于哪个视频 ——
// 后果是**删评论时不会给视频热度 -1**（原项目的已知瑕疵，文档 Q5 记录）。
//
// 阶段5 我们把这个瑕疵修掉了（comment_service.go 的 Delete 里有
// `ChangePopularityTx(-1)`），但那条修正走的是**同步直写路径**：
// 评论对象就在手上，videoID 是免费的。
// **一旦改走 MQ，videoID 就必须由事件带过去** —— 否则那个 -1
// 在异步路径下会直接丢失，而且是静默丢失（消费端不会报错，只是少减一次）。
//
// 教训值得记下来：**事件里要带够上下文，别让消费端去"回查"。**
// 回查意味着消费端要额外访问一次数据库，而且回查那一刻的数据
// 可能已经和事件发生那一刻不同了（评论被改过、视频被删过）。
type CommentEvent struct {
	EventID string `json:"event_id"`
	Action  string `json:"action"` // "publish" / "delete"

	// --- publish 事件用 ---
	Username string `json:"username,omitempty"`
	VideoID  uint   `json:"video_id,omitempty"` // publish: 评论属于哪个视频
	AuthorID uint   `json:"author_id,omitempty"`
	Content  string `json:"content,omitempty"`

	// --- delete 事件用 ---
	CommentID uint `json:"comment_id,omitempty"`

	// 注意 VideoID 是**两种事件共用**的：publish 时是"评论挂在哪个视频下"，
	// delete 时是"要回扣哪个视频的热度"。语义一致，所以一个字段够。

	OccurredAt time.Time `json:"occurred_at"`
}

// NewCommentMQ 建封装。和 NewLikeMQ 逐行同构，含"错误路径手工 Close channel"。
func NewCommentMQ(base *RabbitMQ) (*CommentMQ, error) {
	ch, err := base.NewChannel()
	if err != nil {
		return nil, err
	}
	if err := DeclareTopic(ch, commentExchange, commentQueue, commentBindingKey); err != nil {
		_ = ch.Close()
		return nil, err
	}
	return &CommentMQ{ch: ch}, nil
}

func (c *CommentMQ) Close() error {
	if c == nil || c.ch == nil {
		return nil
	}
	return c.ch.Close()
}

// Publish 发布"发表评论"事件。
//
// **注意它带 username 而不是只带 author_id** —— 这是原项目的做法，
// 理由在 comment_handler.go 里解释过（写时冗余：authorID → username）。
// 消费端要落一条完整的评论行，而评论表里有 username 这个展示字段；
// 让消费端自己回查账号表的话，就又回到"消费端回查"那个坑了。
//
// content 的校验**故意不在这里做**：空的/全空格的评论是**业务规则**
// （"全是空格的评论不算评论"），该在 service 层用 TrimSpace 拦下并给用户
// 明确的报错。MQ 封装这层只拦"畸形到发出去必然让消费端报错"的东西
// （id 为 0、类型不对）。把业务规则放进 MQ 封装，等于让一条注定失败的
// 消息绕一圈才死，还死在一个不报错的地方。
func (c *CommentMQ) Publish(ctx context.Context, username string, videoID, authorID uint, content string) error {
	if c == nil || c.ch == nil {
		return errors.New("comment mq not initialized")
	}
	if videoID == 0 || authorID == 0 {
		return errors.New("video_id and author_id are required")
	}

	eventID, err := newEventID(16)
	if err != nil {
		return err
	}

	return PublishJSON(ctx, c.ch, commentExchange, commentPublishRK, CommentEvent{
		EventID:    eventID,
		Action:     commentActionPublish,
		Username:   username,
		VideoID:    videoID,
		AuthorID:   authorID,
		Content:    content,
		OccurredAt: time.Now().UTC(),
	})
}

// Delete 发布"删除评论"事件。
//
// videoID 是**调用方必须传进来**的（不是在这里查的）——
// 服务端在删除前已经 GetByID 拿到评论对象了，videoID 是免费的；
// 让它再查一次库纯属浪费，而且会引入"查的时候还在、删的时候已被改"
// 的窗口。见 CommentEvent 上关于偏离原项目的说明。
func (c *CommentMQ) Delete(ctx context.Context, commentID, videoID uint) error {
	if c == nil || c.ch == nil {
		return errors.New("comment mq not initialized")
	}
	if commentID == 0 {
		return errors.New("comment_id is required")
	}

	eventID, err := newEventID(16)
	if err != nil {
		return err
	}

	return PublishJSON(ctx, c.ch, commentExchange, commentDeleteRK, CommentEvent{
		EventID:    eventID,
		Action:     commentActionDelete,
		CommentID:  commentID,
		VideoID:    videoID,
		OccurredAt: time.Now().UTC(),
	})
}
