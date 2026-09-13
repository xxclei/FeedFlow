package social

import (
	"log"
	"net/http"

	"myfeed/internal/account"
	"myfeed/internal/apierror"
	"myfeed/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

// SocialHandler 关注模块的 HTTP 层。五个接口**全部**挂在 JWTAuth 下。
//
// 这里有一处和 comment/like handler 不一致的地方，是刻意保留的：
// jwt.GetAccountID 失败时，本文件显式写 `http.StatusUnauthorized`，
// 而 comment_handler/like_handler 用的是 `apierror.ClassifyHTTPStatus(err)` ——
// 后者对裸 error 会兜成 **500**。
//
// 谁对？**这里对**。GetAccountID 失败只可能是"没身份"，那就是 401。
// 但这条路径在 JWTAuth 之后**不可达**（中间件没拿到 accountID 就已经掐断了），
// 所以线上永远不会看到 500。原项目两处写法不同，本文件跟原项目的 social 版。
// 知道这是个纯装饰性的差异就行，不用强行统一 —— 但**要知道哪个才是对的**。
type SocialHandler struct {
	service *SocialService
}

func NewSocialHandler(service *SocialService) *SocialHandler {
	return &SocialHandler{service: service}
}

// Follow POST /social/follow 关注
//
// 请求体 `{vlogger_id}` —— **没有 follower_id**。谁在关注只能从 token 取：
// 让客户端能传 follower_id 等于让任何人替任何人关注。
func (h *SocialHandler) Follow(c *gin.Context) {
	var req FollowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VloggerID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vlogger_id is required"})
		return
	}

	followerID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	// 三种业务错误的码，验收时逐条对：
	//   can not follow self      → 裸 error → 500（原项目如此）
	//   already followed         → 裸 error → 500
	//   （关注不存在的用户）       → gorm.ErrRecordNotFound → **404**
	//   并发重复关注（1062 裸奔）  → 500
	// 全是 4xx 才对，但这是原项目的形态。改进方向见 service.Follow 的注释。
	if err := h.service.Follow(c.Request.Context(), &Social{
		FollowerID: followerID,
		VloggerID:  req.VloggerID,
	}); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "followed"})
}

// Unfollow POST /social/unfollow 取关
func (h *SocialHandler) Unfollow(c *gin.Context) {
	var req UnfollowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VloggerID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vlogger_id is required"})
		return
	}

	followerID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	// 没关注过 → "not followed" 裸 error → 500（**不是**幂等成功，原项目如此）
	if err := h.service.Unfollow(c.Request.Context(), &Social{
		FollowerID: followerID,
		VloggerID:  req.VloggerID,
	}); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unfollowed"})
}

// GetAllFollowers POST /social/getAllFollowers（JWT）粉丝列表
//
// **参数为空的处理是本模块最容易看错的一处**：
//
//	vlogger_id == 0 或没传 → 查"我自己的"粉丝（从 JWT 取）
//	vlogger_id > 0        → 查别人的粉丝列表
//
// 注意这个 0 的含义**只在这两个列表接口成立**。同一个 vlogger_id 在
// follow/unfollow 上，0 是**参数错误 → 400**。同名不同义，改代码时特别容易串。
//
// 未登录时 jwt.GetAccountID 的 err **必须检查**：不检查就会拿着 0 当 ID 去查，
// 于是"未登录"静默变成"查一个不存在的用户"，返回空列表 + 200 —— 看起来一切正常。
func (h *SocialHandler) GetAllFollowers(c *gin.Context) {
	var req GetAllFollowersRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	vloggerID := req.VloggerID
	if vloggerID == 0 {
		accountID, err := jwt.GetAccountID(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		vloggerID = accountID
	}

	followers, err := h.service.GetAllFollowers(c.Request.Context(), vloggerID)
	if err != nil {
		// 目标账号不存在 → gorm.ErrRecordNotFound → 404
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if followers == nil {
		followers = []*account.Account{}
	}

	// 计数是**另一次查询**，和上面的列表不是同一个事务快照 —— 两个并发请求之间
	// 有人关注/取关，就可能出现"列表 3 条、计数 4"的瞬时不一致。原项目接受它，
	// 因为这是展示数据。原项目用 `_` 把错误吞掉（失败静默显示 0），
	// 这里改成记日志（文档建议的改进）—— 线上看到"计数恒为 0"时至少有个线索。
	followerCount, err := h.service.CountFollowers(c.Request.Context(), vloggerID)
	if err != nil {
		log.Printf("CountFollowers 失败: vloggerID=%d, err=%v", vloggerID, err)
	}

	c.JSON(http.StatusOK, GetAllFollowersResponse{
		Followers:     followers,
		FollowerCount: followerCount,
	})
}

// GetAllVloggers POST /social/getAllVloggers（JWT）关注列表。GetAllFollowers 的镜像。
func (h *SocialHandler) GetAllVloggers(c *gin.Context) {
	var req GetAllVloggersRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	followerID := req.FollowerID
	if followerID == 0 {
		accountID, err := jwt.GetAccountID(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		followerID = accountID
	}

	vloggers, err := h.service.GetAllVloggers(c.Request.Context(), followerID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if vloggers == nil {
		vloggers = []*account.Account{}
	}

	vloggerCount, err := h.service.CountVloggers(c.Request.Context(), followerID)
	if err != nil {
		log.Printf("CountVloggers 失败: followerID=%d, err=%v", followerID, err)
	}

	c.JSON(http.StatusOK, GetAllVloggersResponse{
		Vloggers:     vloggers,
		VloggerCount: vloggerCount,
	})
}

// GetCounts POST /social/getCounts（JWT）我的关注计数总览
//
// **不绑定请求体**：身份全在 token 里，请求体是空的 `{}`。
// 绑了的话，gin 对空 body 会返回 EOF → 把"看我的计数"变成一个 400。
// 和 like_handler.ListMyLikedVideos 同一个理由。
func (h *SocialHandler) GetCounts(c *gin.Context) {
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	// 两次 COUNT 之间也没有事务快照，同上：瞬时不一致可以接受
	followerCount, err := h.service.CountFollowers(c.Request.Context(), accountID)
	if err != nil {
		log.Printf("CountFollowers 失败: accountID=%d, err=%v", accountID, err)
	}
	vloggerCount, err := h.service.CountVloggers(c.Request.Context(), accountID)
	if err != nil {
		log.Printf("CountVloggers 失败: accountID=%d, err=%v", accountID, err)
	}

	c.JSON(http.StatusOK, SocialCounts{
		FollowerCount: followerCount,
		VloggerCount:  vloggerCount,
	})
}
