package notification

import (
	"context"

	"gorm.io/gorm"
)

// NotificationRepository 只有 Create 一个方法，看着单薄，但它是**必要的一层**。
//
// 原项目在 comment_service.go 里是这么写的：
//
//	s.repo.db.Table("notifications").Create(&notif)
//
// 三个问题叠在一起：评论的 repo 知道了 notifications 的表名、绕过了自己的封装、
// 还硬写了一个匿名 struct（字段漏一个就静默少写一列，不报错）。文档把它记作
// "分层妥协，心里有数"。我们这里不妥协：视频包不该知道通知表的形状。
//
// 阶段10 会往这里加 ListUnread / MarkRead / MarkAllRead —— 那时这层就不是"为了一个
// 方法开一个包"了，它本来就要长成这样。
type NotificationRepository struct {
	db *gorm.DB
}

func NewNotificationRepository(db *gorm.DB) *NotificationRepository {
	return &NotificationRepository{db: db}
}

// Create 写一条通知。
//
// **刻意不提供 Tx 版本**：这个写入永远在评论事务**之外**（见 comment_service.go
// 里 notifyMentions 的调用位置）—— 它不是"评论发布"这件事的一部分，
// 而是它成功之后的副作用。副作用和主流程共用一个事务，等于宣布
// "通知写不进去，评论也发不出去"，那正是我们要避免的。
func (r *NotificationRepository) Create(ctx context.Context, n *Notification) error {
	return r.db.WithContext(ctx).Create(n).Error
}
