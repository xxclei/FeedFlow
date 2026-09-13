// Package social 关注模块（阶段6）。
//
// 这个包拥有全项目最"薄"的一张表，却承担着最多的连接：feed 的关注流读它、
// getProfile 的计数读它、本模块的四个列表/计数接口读它。所以下面这张表的
// 字段名一个都不能改 —— 改了要同时动四处（见本文件末尾的说明）。
package social

import "myfeed/internal/account"

// Social 关注关系：一行 = "FollowerID 关注了 VloggerID" 这一条**有向边**。
//
// 自引用设计：两个 ID 都逻辑指向 accounts.id，但**不加 GORM 外键标签**，
// 数据库层没有任何 FK 约束 —— "这两列指向的账号一定存在"这件事由 service 校验兜
// （Follow 里两次 FindByID）。和 likes/comments 一样，我们手工模拟外键。
//
// 三列存下整个关注图。没有 CreatedAt 是原项目的形态，两个后果都要知道：
//
//	① 无法知道"何时关注"，所以粉丝列表只能按主键序返回（≈关注先后）——
//	   这是**近似**，不是保证，见 repo.go 里关于 IN 不保序的说明
//	② 阶段9 若要做"最近关注了你"的排序，得先加列 + 回填，改不动历史数据
type Social struct {
	ID uint `gorm:"primaryKey"`

	// 三个索引，别混：
	//
	//	idx_social_follower_vlogger (follower_id, vlogger_id) —— 复合**唯一**索引，
	//	   防重复关注的唯一防线（和点赞的 idx_like_video_account 同款"约束层"）。
	//	   注意 GORM 的写法：两个字段分别打 `uniqueIndex:同名`，生成的是**一个**
	//	   复合索引，列顺序 = 字段**声明**顺序（所以 FollowerID 必须在前面 ——
	//	   顺序反了最左前缀就换了方向）。迁移后用
	//	   `show index from socials` 确认它是一个两列索引，不是两个单列索引。
	//
	//	idx_social_follower —— **严格冗余**：复合唯一索引的最左前缀已经覆盖
	//	   "按 follower_id 查"。原项目显式建了，无害但每条关注多维护一棵 B+ 树。
	//	   留着是对齐，删掉也不会有查询变慢。（comments.username 索引是同一种东西。）
	FollowerID uint `gorm:"not null;index:idx_social_follower;uniqueIndex:idx_social_follower_vlogger"`

	// idx_social_vlogger —— **不冗余，而且必需**：反向查询"谁关注了我"
	//   （GetAllFollowers）和 CountFollowers 都按 vlogger_id 过滤，
	//   复合索引的最左前缀是 follower_id，走不了。
	//   这就是"双向查询要双向索引"的最小例子。
	VloggerID uint `gorm:"not null;index:idx_social_vlogger;uniqueIndex:idx_social_follower_vlogger"`
}

// 别把 Social 当无主之表删字段：feed 的子查询（author_id IN SELECT vlogger_id ...）、
// getProfile 的两个计数、本模块的四个接口全指着 FollowerID/VloggerID 这两列。

type FollowRequest struct {
	VloggerID uint `json:"vlogger_id"`
}

type UnfollowRequest struct {
	VloggerID uint `json:"vlogger_id"`
}

// GetAllFollowersRequest 的 VloggerID 可以是 0 —— 语义是"查我自己"。
// 这个"0 = 从 JWT 取自己"的约定只在这两个列表接口上有，
// follow/unfollow 的 0 是**参数错误**（400）。同一个字段名，两种 0 的含义，
// 是本模块最容易看错的一处，见 handler.go。
type GetAllFollowersRequest struct {
	VloggerID uint `json:"vlogger_id"`
}

type GetAllVloggersRequest struct {
	FollowerID uint `json:"follower_id"`
}

// GetAllFollowersResponse / GetAllVloggersResponse 的元素是**完整 Account JSON**。
// 看结构体是 *account.Account（带着 Password/Token/RefreshToken 字段），
// 但序列化出去不会泄露：那三个字段在 account.Account 上都打了 `json:"-"`。
// 这是"实体复用做响应"能成立的前提，也说明那三个 `json:"-"` 不是装饰品。
type GetAllFollowersResponse struct {
	Followers     []*account.Account `json:"followers"`
	FollowerCount int64              `json:"follower_count"`
}

type GetAllVloggersResponse struct {
	Vloggers     []*account.Account `json:"vloggers"`
	VloggerCount int64              `json:"vlogger_count"`
}

// SocialCounts 身份只从 JWT 取，请求体是空的 `{}`。
type SocialCounts struct {
	FollowerCount int64 `json:"follower_count"`
	VloggerCount  int64 `json:"vlogger_count"`
}
