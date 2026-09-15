// Package notification 通知模块（阶段5 只落"表 + 写入"这一小块）。
//
// 原项目把 Notification 实体放在 internal/worker 里（notificationworker.go），
// 因为它的主要消费者是阶段10 的 MQ Worker + SSE 推送。但**实体本身不是 worker 的东西**
// —— 它是这张表的形状，而第一个写它的人是阶段5 的评论区（@提及）。
//
// 所以这里单开一个包放实体，理由有两条：
//
//	① 依赖方向干净：video 包 import notification（评论要写通知），
//	   而不是 import worker（一个视频模块去认识 MQ 消费者，说不通）
//	② 阶段10 有家可回：Worker / SSE Hub 到时候写在这个包里，
//	   实体不用再搬一次家（搬家意味着所有 import 路径都改）
package notification

import "time"

// 通知类型。用常量而不是到处散字符串字面量 —— 阶段10 的 SSE 推送要按 Type 分流，
// 那时拼错一个 "mention" 不会编译报错，只会在运行时静默不推。
const (
	TypeMention = "mention" // 评论里 @ 了你（阶段5，同步写，不走 MQ）
	TypeLike    = "like"    // 点赞了你的视频（阶段9，走 MQ）
	TypeComment = "comment" // 评论了你的视频（阶段9，走 MQ）
	TypeFollow  = "follow"  // 关注了你（阶段9，走 MQ）

	// 为什么 like/comment/follow 三个走 MQ，而 mention 不走：
	// 前三者的触发动作本身就是异步的（点赞/评论落库在 worker 里），
	// 通知顺手在同一个消费流程里写掉是**零额外成本**；
	// 而 mention 是评论内容的一部分 —— 它要知道"这条评论里 @ 了谁"，
	// 那是**评论正文的语义**，MQ 事件里得把解析结果或者原文带过去。
	// 原项目选了"留在同步路径里直接查账号表 + INSERT"。
	//
	// 代价是一处不一致（comment_service.go 里已如实记下）：
	// 走 MQ 时评论还没落库，mention 通知却已经发出去了。
)

// Notification 一条通知。表名 notifications（GORM 按结构体复数推导）。
//
// 这张表是**收件箱模型**：一行 = "发给 RecipientID 的一条消息"，
// SenderID 是动作发起人，TargetID 是动作对象（阶段5 是 videoID，
// 阶段9 的 follow 通知里会是 vloggerID）。Content 存渲染好的文案。
type Notification struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	RecipientID uint      `gorm:"index;not null" json:"recipient_id"`
	SenderID    uint      `gorm:"not null" json:"sender_id"`
	Type        string    `gorm:"type:varchar(50);not null" json:"type"`
	TargetID    uint      `json:"target_id"`
	Content     string    `gorm:"type:varchar(255)" json:"content"`
	IsRead      bool      `gorm:"default:false" json:"is_read"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// 两个索引上的取舍（阶段5 只写不读，所以现在看不出差别，阶段10 会显形）：
//
//	recipient_id 有索引      —— 收件箱的主查询是"拉我的通知"，必须走它
//	sender_id 没有索引       —— 没有"我发过哪些通知"这种查询。
//	                            但阶段9 的 follow 通知如果要做"某人关注了你"的聚合，
//	                            会需要 (recipient_id, sender_id) 的复合索引
//	is_read 没有索引         —— 未读数统计要 COUNT(*) WHERE recipient_id=? AND is_read=0，
//	                            复合索引 (recipient_id, is_read) 才是正解，单列没用
//
// 原项目这三条一模一样。照抄的理由是：阶段5 没有读路径，加索引纯属猜。
// 等阶段10 真正有了"收件箱"接口，按 EXPLAIN 的结果再加，比现在拍脑袋准。
