package feed

import (
	"time"

	"myfeed/internal/search"
)

// FeedAuthor 作者信息（写时冗余，从 videos 表直接带出，不用查 accounts）
type FeedAuthor struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
}

// FeedVideoItem Feed 里的单条视频视图（比 video.Video 多了 is_liked，少了内部字段）
type FeedVideoItem struct {
	ID          uint       `json:"id"`
	Author      FeedAuthor `json:"author"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	PlayURL     string     `json:"play_url"`
	CoverURL    string     `json:"cover_url"`
	CreateTime  int64      `json:"create_time"` // **Unix 秒**（给前端 formatTime(sec) 用的展示值，不是游标）
	LikesCount  int64      `json:"likes_count"`
	IsLiked     bool       `json:"is_liked"` // 当前用户是否点过赞（阶段4接入，现在恒false）
}

// ---- 最新流：时间游标 ----

type ListLatestRequest struct {
	Limit      int   `json:"limit"`
	LatestTime int64 `json:"latest_time"` // 上一页最后一条的 create_time(毫秒)；首页传0
}

type ListLatestResponse struct {
	VideoList []FeedVideoItem `json:"video_list"`
	NextTime  int64           `json:"next_time"` // 下一页带着它来
	HasMore   bool            `json:"has_more"`
}

// ---- 点赞榜：复合游标（热度+ID 双键，防同热度跳页） ----

type ListLikesCountRequest struct {
	Limit            int    `json:"limit"`
	LikesCountBefore *int64 `json:"likes_count_before,omitempty"` // 指针！区分"没传"和"传0"
	IDBefore         *uint  `json:"id_before,omitempty"`
}

// LikesCountCursor 内部用的游标二元组
type LikesCountCursor struct {
	LikesCount int64
	ID         uint
}

type ListLikesCountResponse struct {
	VideoList            []FeedVideoItem `json:"video_list"`
	NextLikesCountBefore *int64          `json:"next_likes_count_before,omitempty"`
	NextIDBefore         *uint           `json:"next_id_before,omitempty"`
	HasMore              bool            `json:"has_more"`
}

// ---- 关注流（阶段6回填：路由挂 JWTAuth，这里先备好结构体） ----

type ListByFollowingRequest struct {
	Limit      int   `json:"limit"`
	LatestTime int64 `json:"latest_time"`
}

type ListByFollowingResponse struct {
	VideoList []FeedVideoItem `json:"video_list"`
	NextTime  int64           `json:"next_time"`
	HasMore   bool            `json:"has_more"`
}

// ---- 热榜（阶段8 会把主路径换成 Redis 快照；本阶段先做 DB 三键游标兜底） ----

type ListByPopularityRequest struct {
	Limit          int   `json:"limit"`
	AsOf           int64 `json:"as_of"`  // 服务器返回的分钟时间戳；第一页传0
	Offset         int   `json:"offset"` // 下一页从这里开始；第一页传0
	LatestIDBefore *uint `json:"latest_id_before,omitempty"`

	// DB 兜底游标（Redis 快照挂掉时的降级通道，阶段8启用）
	LatestPopularity int64     `json:"latest_popularity"`
	LatestBefore     time.Time `json:"latest_before"`
}

type ListByPopularityResponse struct {
	VideoList  []FeedVideoItem `json:"video_list"`
	AsOf       int64           `json:"as_of"`
	NextOffset int             `json:"next_offset"`
	HasMore    bool            `json:"has_more"`

	NextLatestPopularity *int64     `json:"next_latest_popularity,omitempty"`
	NextLatestBefore     *time.Time `json:"next_latest_before,omitempty"`
	NextLatestIDBefore   *uint      `json:"next_latest_id_before,omitempty"`
}

// ---- 混合检索（词法 + 语义向量 + RRF 融合） ----

type SearchRequest struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"` // 上一页返回的 next_cursor，**原样回传**；首页不传，或传空串
}

// SearchResponse 搜索响应。
//
// ---------- 游标为什么是字符串 ----------
//
// 其它四条流的游标都是一组数值（时间戳/计数/id 的二元组三元组），前端要把它们
// 原样存好再传回来。搜索不一样：融合结果的"位置"不是任何一列的取值，
// 而是一份**冻结的排名快照**（理由见 search/cursor.go）。
// 所以这里给前端的是一个不透明字符串 —— 前端不需要、也没法理解它的内容，
// 这也正好让"以后换分页实现"不需要改前端。
//
// **注意翻页时查询词仍然要照传**（或者不传也行，见 handler 的说明）：
// 服务端翻页不看它，但带着它能让日志/埋点里的上下文是完整的。
type SearchResponse struct {
	VideoList  []FeedVideoItem `json:"video_list"`
	NextCursor string          `json:"next_cursor,omitempty"`
	HasMore    bool            `json:"has_more"`

	// Mode 本次实际跑成的形态（search.Mode 的说明写了每个取值的含义）。
	//
	// Arms / Total 和 Mode 一起构成**可观测性**：搜索"变差了"的时候，
	// 这三个字段能立刻回答"是哪一路塌了、还是两路都跑了只是没命中"。
	// 前端把它们放进调试面板 —— 这一页最有价值的不是搜索结果，是这三个数字。
	Mode  search.Mode `json:"mode"`
	Arms  search.Arms `json:"arms"`
	Total int         `json:"total"` // 冻结列表的总长度（可翻的候选数，不是精确命中数）
}
