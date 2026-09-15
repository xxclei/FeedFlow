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

// ---------- 阶段9 加的读路径 ----------
//
// repo.go 顶上那句"阶段10 会往这里加 ListUnread / MarkRead"提前兑现了：
// SSE 的四个接口（list / markRead / unreadCount）需要它们，而阶段9 的
// 通知推送**本来就要有个地方把通知查出来**——推送只送新增的，
// 用户刷新页面后看到的收件箱得从表里读。

// ListByRecipient 拉某个用户的通知，**最新的在前，最多 50 条**。
//
// 排序用 (created_at DESC, id DESC) 双键而不是只按 created_at：
// created_at 的精度是秒，同一个 SSE 推来的两条通知极可能同秒，
// 只按时间排的话这两条的顺序是**数据库实现相关的**（不稳定排序），
// 翻两次页可能拿到不同的顺序。加 id 当第二键，顺序就定死了 ——
// 和阶段3 游标分页里"时间戳不唯一必须补主键"是同一个道理。
//
// LIMIT 50 是写死的上限，没有游标 —— 原项目如此。**第 51 条通知拿不到，
// 而且用户看不出来自己漏了**（列表就是变短了）。要做对得加游标分页，
// 那是阶段3 已经练过的东西，本项目不在这里重做一遍。
func (r *NotificationRepository) ListByRecipient(ctx context.Context, recipientID uint, limit int) ([]Notification, error) {
	var list []Notification
	err := r.db.WithContext(ctx).
		Where("recipient_id = ?", recipientID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&list).Error
	return list, err
}

// MarkRead 把某一条通知标为已读，**带上 recipient_id 条件**。
//
// 这个条件是安全线，不是优化：不带它的话，任何登录用户只要猜一个
// notification id 就能把**别人**的通知标成已读（"标记已读"这个动作
// 在界面上无感，但它是越权写）。原项目这里是 `db.Model().Where("id = ?")`，
// 只有 id。**本项目补上 recipient_id —— 这是一处刻意的偏离，方向是收紧。**
//
// 返回 RowsAffected 的 bool：0 行说明"这条通知不存在或者不是你的"，
// 两种情况的对外表现应该一样（都是 404/无变化），不该让攻击者靠
// 返回值的差异区分出"id 存在但不属于我"和"id 根本不存在"。
func (r *NotificationRepository) MarkRead(ctx context.Context, id, recipientID uint) (bool, error) {
	res := r.db.WithContext(ctx).
		Model(&Notification{}).
		Where("id = ? AND recipient_id = ?", id, recipientID).
		Update("is_read", true)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// MarkAllRead 把某个用户的**所有未读**标为已读。
//
// `is_read = false` 这个条件在语义上是多余的（已经是 true 的再写一遍 true
// 结果一样），但它在**性能**上不是：不带它的话每次"全部已读"都要
// 重写一遍全部历史通知（用户有 1000 条通知就写 1000 行，哪怕只有 2 条未读）。
// 带上它，正常情况下写的是 0~几条。
//
// 这是"幂等写"里一个常被忽略的点：**幂等描述的是结果，不代表可以不做限制地写。**
func (r *NotificationRepository) MarkAllRead(ctx context.Context, recipientID uint) (int64, error) {
	res := r.db.WithContext(ctx).
		Model(&Notification{}).
		Where("recipient_id = ? AND is_read = false", recipientID).
		Update("is_read", true)
	return res.RowsAffected, res.Error
}

// CountUnread 未读数。
//
// 走的是 idx_recipient（recipient_id 那个索引）—— 先按收件人过滤，
// 再在结果里筛 is_read。entity.go 里说过正确的索引是复合的
// (recipient_id, is_read)，本项目**不加**：通知量小的时候 EXPLAIN
// 看不出差别，而"按 EXPLAIN 的结果加索引"才是正确的做法
// —— 现在加属于拍脑袋，和阶段5 留的说明一致。
func (r *NotificationRepository) CountUnread(ctx context.Context, recipientID uint) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).
		Model(&Notification{}).
		Where("recipient_id = ? AND is_read = false", recipientID).
		Count(&count).Error
	return count, err
}
