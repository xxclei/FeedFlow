package video

import "time"

// Comment 一条评论。
//
// 和 Like 对比着看，才看得出这张表为什么长这样（阶段4 vs 阶段5 的差异全在这）：
//
//	             Like                        Comment
//	唯一索引      (video_id, account_id)      没有
//	1062 分支     有（并发双击的兜底防线）      没有（评论天然可重复，一人能发多条）
//	幂等性        数据库级幂等                 无幂等（重复提交就是两条）
//	删除          按 (video_id, account_id) 删 先 GetByID 查出来、校验属主、再按主键删
//	写时冗余      ChangeLikesCount(+1) 计数     Username 字符串
//
// 最后一行是这两个模块最像的地方，但冗余的东西不同：点赞冗余的是**聚合值**
// （likes_count，为了读的时候不用 COUNT），评论冗余的是**展示字段**
// （username，为了读的时候不用 JOIN accounts）。
// 两者共用的判断标准是同一条：**这个值是不是读得比写得频繁得多**。
type Comment struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	Username string `gorm:"index" json:"username"`

	// 冗余的代价和 videos.username 一模一样：用户改名后，历史评论里还是旧名字。
	// 前端 AuthorCard.vue 已经为同一件事写过注释（"videos.username 是发布时抄的快照"）。
	// 这里是有意接受：评论是**历史快照**，"他当时叫这个名字"本身就是正确信息。
	//
	// 想改成读时补齐的话，注意两点：listAll 会从"零 JOIN"变成"一次批量 IN"
	// （不是 N+1，但也不是免费），而且补出来的是**当前**名字 —— 语义变了，
	// 不是单纯的性能改动。
	//
	// 为什么带 index：原项目显式建了。实际上没有任何查询按 username 过滤
	// （listAll 只按 video_id），所以它是**冗余索引**，每条评论都要多维护一棵 B+ 树。
	// 和 socials 表的 idx_social_follower 是同一种"照抄留下的冗余"（阶段6 讲）。
	// 保留的理由是它在原项目里就在，删掉会让你以后对照 diff 时多一个干扰项。

	VideoID   uint      `gorm:"index" json:"video_id"`
	AuthorID  uint      `gorm:"index" json:"author_id"` // 删除权限的判断依据，不是 username
	Content   string    `gorm:"type:text" json:"content"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// AuthorID 为什么是权限依据而不是 Username：username 是可变的、可重复度高的展示字段，
// 拿它做鉴权等于用"名字"当"身份证"。前端传上来的请求体里**只有 comment_id**，
// 一个身份字段都没有 —— 身份只能从 JWT 取，这是整条安全线的根。

type PublishCommentRequest struct {
	VideoID uint   `json:"video_id"`
	Content string `json:"content"`
}

type DeleteCommentRequest struct {
	CommentID uint `json:"comment_id"`
}

type GetAllCommentsRequest struct {
	VideoID uint `json:"video_id"`
}
