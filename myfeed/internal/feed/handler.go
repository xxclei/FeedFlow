package feed

import (
	"time"

	"myfeed/internal/apierror"
	"myfeed/internal/middleware/jwt"

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

// nonNilFeedVideoItems 把 nil 切片换成长度 0 的切片。
// 原因：nil 切片序列化成 JSON 是 null，空切片是 []。
// 前端 v-for 遇到 null 会炸，遇到 [] 什么都不渲染——所以要在这里兜住
func nonNilFeedVideoItems(items []FeedVideoItem) []FeedVideoItem {
	if items == nil {
		return []FeedVideoItem{}
	}
	return items
}
