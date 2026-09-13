package video

import "time"

// Like 点赞流水：一条记录 = "谁在什么时候赞了哪个视频"。
//
// 它是本模块的**真相源**：videos.likes_count / videos.popularity 只是由它派生的
// 冗余计数。这个分离决定了容错方向 —— 计数写歪了还有救（照流水重算），
// 流水丢了才叫丢数据。进阶项里的"漂移自愈"就是照着这一点做的。
//
// 复合唯一索引 (video_id, account_id) 是模块基石：一人一赞这件事不靠应用层自觉，
// 靠数据库拒绝。并发双击时两个请求都能通过 service 的预检，但第二个 INSERT
// 必然撞 1062 —— 那才是防线，预检只是体验。
//
// 两个字段的 tag 名必须**完全一致**，否则 AutoMigrate 会建成两个单列索引
// （= 每人只能赞一个视频、每个视频只能被一个人赞）。写完用 show index from likes 验。
type Like struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	VideoID   uint `gorm:"uniqueIndex:idx_like_video_account;not null" json:"video_id"`
	AccountID uint `gorm:"uniqueIndex:idx_like_video_account;not null" json:"account_id"`

	// CreatedAt 由 service 显式赋值，不依赖 GORM 的填充。
	// 注意 GORM 对名为 CreatedAt 的字段**默认就会自动填充**（不必写 autoCreateTime tag），
	// 所以这里的坑不是"忘了写 tag"，而是"以为它一定是当前时刻"：阶段9 的 Worker
	// 从 MQ 消息里拿到的是事件发生时刻 occurred_at，那时必须能覆盖这个值。
	// 它同时是"我的点赞列表"的排序依据（ORDER BY likes.created_at DESC）。
	CreatedAt time.Time `json:"created_at"`
}

// LikeRequest 三个写/查接口（like / unlike / isLiked）的请求体。
//
// 只有 VideoID，**没有 AccountID**：身份一律从 token 取（jwt.GetAccountID），
// 让客户端能传 account_id 等于把"代表谁点赞"的权力交出去。
//
// 刻意不加 `binding:"required"`：那样 video_id 缺失或为 0 会被 gin 拦下并返回
// 一句 Go 的英文错误，而我们要的是统一业务文案 "video_id is required"。
// 所以这条校验在 handler 里手写判断（对照文档的错误码表）。
//
// listMyLikedVideos **无请求体**，所以它不 bind 这个结构体。
type LikeRequest struct {
	VideoID uint `json:"video_id"`
}
