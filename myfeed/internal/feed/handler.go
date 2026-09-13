package feed

import (
	"errors"
	"time"

	"myfeed/internal/apierror"
	"myfeed/internal/middleware/jwt"
	"myfeed/internal/search"

	"github.com/gin-gonic/gin"
)

// FeedHandler HTTP 层：三件事
//
//	① limit 归一化（脏输入就地修，不进 service）
//	② 游标解码 + 完整性校验（毫秒→time.Time；成对参数必须成对出现）
//	③ 匿名降级（拿不到身份就当游客，viewerAccountID = 0）
//
// 阶段6回填：ListByFollowing（依赖 social 包）
type FeedHandler struct {
	service *FeedService
}

func NewFeedHandler(service *FeedService) *FeedHandler {
	return &FeedHandler{service: service}
}

// ListLatest 最新流。路由挂 SoftJWTAuth：登录/未登录都能调
func (f *FeedHandler) ListLatest(c *gin.Context) {
	var req ListLatestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.Limit <= 0 || req.Limit > 50 {
		req.Limit = 10
	}

	// 游标解码：毫秒时间戳 → time.Time。0 = 首页，保持零值让 repo 跳过过滤
	var latestTime time.Time
	if req.LatestTime > 0 {
		latestTime = time.UnixMilli(req.LatestTime)
	}

	// 匿名降级：软鉴权下没 token 是正常情况，不是错误
	viewerAccountID, err := jwt.GetAccountID(c)
	if err != nil {
		viewerAccountID = 0
	}

	resp, err := f.service.ListLatest(c.Request.Context(), req.Limit, latestTime, viewerAccountID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, resp)
}

// ListLikesCount 点赞榜。游标是 (likes_count, id) 二元组，必须成对
func (f *FeedHandler) ListLikesCount(c *gin.Context) {
	var req ListLikesCountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.Limit <= 0 || req.Limit > 50 {
		req.Limit = 10
	}

	// 这段校验是原项目里最绕的一块，目标是：把"客户端原样回传的零值游标"
	// 和"真正的非法游标"区分开。逐条拆：
	//   两个都不传        → 首页，cursor = nil
	//   只传一个          → 400（成对参数）
	//   likes_count < 0   → 400
	//   id == 0 且 likes != 0 → 400（榜单里 id 恒 > 0，这是明显的坏数据）
	//   id == 0 且 likes == 0 → 首页。因为游标字段带 omitempty，
	//                         客户端把上一页的零值游标回传时会变成"两个都没传"
	var cursor *LikesCountCursor
	if req.LikesCountBefore != nil || req.IDBefore != nil {
		if req.LikesCountBefore == nil || req.IDBefore == nil {
			c.JSON(400, gin.H{"error": "likes_count_before and id_before must be provided together"})
			return
		}

		likesCountBefore := *req.LikesCountBefore
		idBefore := *req.IDBefore

		if likesCountBefore < 0 {
			c.JSON(400, gin.H{"error": "invalid cursor: likes_count_before must be >= 0"})
			return
		}
		if idBefore == 0 {
			if likesCountBefore != 0 {
				c.JSON(400, gin.H{"error": "invalid cursor: id_before must be > 0"})
				return
			}
			// 走到这里 = (0, 0)：当作没游标，直接查第一页
		} else {
			cursor = &LikesCountCursor{
				LikesCount: likesCountBefore,
				ID:         idBefore,
			}
		}
	}

	viewerAccountID, err := jwt.GetAccountID(c)
	if err != nil {
		viewerAccountID = 0
	}

	resp, err := f.service.ListLikesCount(c.Request.Context(), req.Limit, cursor, viewerAccountID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, resp)
}

// ListByPopularity 热榜。
// 本阶段走 DB 三键游标；AsOf/Offset 是给阶段8 Redis 快照预留的空壳参数
func (f *FeedHandler) ListByPopularity(c *gin.Context) {
	var req ListByPopularityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.Limit <= 0 || req.Limit > 50 {
		req.Limit = 10
	}

	viewerAccountID, err := jwt.GetAccountID(c)
	if err != nil {
		viewerAccountID = 0
	}

	var latestPopularity int64
	var latestBefore time.Time
	var latestIDBefore uint

	// popularity 允许为 0，所以判"有没有游标"只能看另外两个键
	if req.LatestPopularity < 0 {
		c.JSON(400, gin.H{"error": "latest_popularity must be >= 0"})
		return
	}

	// 和点赞榜同一套纪律：游标要么全给，要么全不给。
	// 只给一半说明客户端只翻了一半的书签，继续查会静默返回错页
	anyCursor := !req.LatestBefore.IsZero() || req.LatestIDBefore != nil
	if anyCursor {
		if req.LatestBefore.IsZero() || req.LatestIDBefore == nil || *req.LatestIDBefore == 0 {
			c.JSON(400, gin.H{"error": "latest_before and latest_id_before must be provided together"})
			return
		}
		latestPopularity = req.LatestPopularity
		latestBefore = req.LatestBefore
		latestIDBefore = *req.LatestIDBefore
	}

	resp, err := f.service.ListByPopularity(
		c.Request.Context(),
		req.Limit,
		req.AsOf,
		req.Offset,
		viewerAccountID,
		latestPopularity,
		latestBefore,
		latestIDBefore,
	)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, resp)
}

// ListByTag 标签流。无游标（阶段3简化版），只取最新 N 条
func (f *FeedHandler) ListByTag(c *gin.Context) {
	var req struct {
		TagName string `json:"tag_name"`
		Limit   int    `json:"limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.TagName == "" {
		c.JSON(400, gin.H{"error": "tag_name is required"})
		return
	}
	if req.Limit <= 0 || req.Limit > 50 {
		req.Limit = 10
	}

	// 注意这里用了 _：标签流是纯公开接口，游客身份对结果没有影响
	viewerAccountID, _ := jwt.GetAccountID(c)

	items, err := f.service.ListByTag(c.Request.Context(), req.TagName, req.Limit, viewerAccountID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"video_list": nonNilFeedVideoItems(items)})
}

// ListByFollowing 关注流（阶段6）。
//
// **本文件和项目里其它 handler 最不一样的一点：身份取不到时 401，不降级。**
//
// 上面四个 List* 都写着 `viewerAccountID, err := jwt.GetAccountID(c); if err != nil { viewerAccountID = 0 }`
// —— 那是因为它们挂在 **SoftJWTAuth** 下，没 token 是正常情况，降级成游客是对的。
//
// 这个 handler 不行。viewerAccountID = 0 在 repo 里的含义是"**不过滤**"，
// 也就是全站最新流。降级 = 把"关注流"悄悄换成"全站流"，并且：
//
//	不报错、不返回 403、响应结构完全一样、看起来就是一个正常的 feed
//
// 只有内容不对（里面全是没关注的人）。这是那种"测试环境永远发现不了"的 bug。
//
// 原项目的这一行是 `if err != nil { viewerAccountID = 0 }`，靠"路由挂了 JWTAuth
// 所以不可达"来兜底。这里改成显式 401：**让不可达的路径也走在安全的一侧**，
// 而不是依赖另一个文件里的中间件配置永远正确。
//
// 顺带：这里不写 `if viewerAccountID == 0 { 401 }` 这种二次校验 ——
// GetAccountID 的 err 已经覆盖了"没身份"，再加一层会让"accountID 真的是 0"
// 这种不存在的情况显得可能发生。
func (f *FeedHandler) ListByFollowing(c *gin.Context) {
	var req ListByFollowingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.Limit <= 0 || req.Limit > 50 {
		req.Limit = 10
	}

	// 游标解码：**毫秒**，和 ListLatest 一致（原项目这里是 time.Unix(秒)，
	// 刻意改的 —— 理由见 feed/service.go 的 ListByFollowing）
	var latestTime time.Time
	if req.LatestTime > 0 {
		latestTime = time.UnixMilli(req.LatestTime)
	}

	viewerAccountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(401, gin.H{"error": err.Error()})
		return
	}

	resp, err := f.service.ListByFollowing(c.Request.Context(), req.Limit, latestTime, viewerAccountID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	resp.VideoList = nonNilFeedVideoItems(resp.VideoList)
	c.JSON(200, resp)
}

// Search 混合检索（阶段7）。路由挂 **SoftJWTAuth**：游客也能搜。
//
// 为什么是软鉴权而不是强鉴权：搜索的内容是**公开数据**，没有任何东西是按
// 观察者变的有序性（不像关注流 —— 那个的 viewerAccountID = 0 意味着"不过滤"，
// 是安全相关的一行，见 ListByFollowing 的长注释）。
// 这里取不到身份就是 is_liked 全是 false，正是游客该看到的。
//
// ---------- 和上面四个 handler 最不一样的一点：游标解读失败返回 400 ----------
//
// 别的流的游标是数字，脏了最多是"查出来的东西不对"。搜索的游标是一个
// **冻结排名的令牌**，它脏了无法"尽力而为" —— 里面就是下一页的全部内容。
// 所以解码失败一律 400，并且前端应该把这理解为"**从头搜一次**"，
// 而不是重试同一个令牌（重试一万次都是同样的 400）。
// 什么情况会脏：格式变了（换了实现）、令牌被截断、用户把上一次部署的令牌留在
// 了 localStorage 里。
func (f *FeedHandler) Search(c *gin.Context) {
	var req SearchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.Limit <= 0 || req.Limit > 50 {
		req.Limit = 10
	}

	// 先解游标。**翻页时不需要查询词**（召回根本不跑，用的是冻结列表），
	// 所以 req.Query 为空 + 有游标是合法的翻页请求 —— 这一点和
	// "第一页必须给查询词"不一样，不能一刀切地校验 query 非空
	var cur *search.Cursor
	if req.Cursor != "" {
		decoded, err := search.DecodeCursor(req.Cursor)
		if err != nil {
			c.JSON(400, gin.H{"error": "游标无效，请重新搜索: " + err.Error()})
			return
		}
		cur = &decoded
	}

	// 编译查询。只在第一页做 —— 翻页时传了也没用（结果集已经冻住了），
	// 这里刻意**不报错**：前端图省事把 query 一起带上是很自然的写法，
	// 为此回 400 属于自找麻烦
	var q search.Query
	if cur == nil {
		compiled, err := search.CompileQuery(req.Query)
		if err != nil {
			var unsearchable *search.UnsearchableError
			if errors.As(err, &unsearchable) {
				// 只打了标点。这是**请求形状问题**，回 400；
				// 而且**绝不能返回空列表** —— 空列表的语义是"没有这个视频"，
				// 前端会显示"没有找到相关视频"，而事实是这个查询根本没执行
				c.JSON(400, gin.H{"error": unsearchable.Error()})
				return
			}
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		q = compiled
	}

	// 用 `_` 忽略 err：游客搜索是正常场景，理由同 ListByTag
	viewerAccountID, _ := jwt.GetAccountID(c)

	resp, err := f.service.Search(c.Request.Context(), q, cur, req.Limit, viewerAccountID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	resp.VideoList = nonNilFeedVideoItems(resp.VideoList) // 和上面五条流同一条纪律
	c.JSON(200, resp)
}

// nonNilFeedVideoItems 把 nil 切片换成长度 0 的切片。
// 原因：nil 切片序列化成 JSON 是 null，空切片是 []。
// 前端 v-for 遇到 null 会炸，遇到 [] 什么都不渲染——所以要在这里兜住
func nonNilFeedVideoItems(items []FeedVideoItem) []FeedVideoItem {
	if items == nil {
		return []FeedVideoItem{}
	}
	return items
}
