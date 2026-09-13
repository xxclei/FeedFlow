package video

import (
	"net/http"

	"myfeed/internal/apierror"
	"myfeed/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

// LikeHandler 点赞模块的 HTTP 层。四个接口全部挂在 jwt.JWTAuth 分组下。
//
// 三件事在这里做，一件都不下放：
//
//	① 参数校验（video_id 缺失/为 0 → 400）—— 脏输入就地修，不进 service
//	② 身份提取（accountID 一律从 token 取，请求体里没有 account_id）
//	③ 响应形状兜底（空列表转 []Video{}，不让前端收到 null）
//
// 错误码分工：**参数问题 = 400**（这里手写）；**业务问题 = 500**（原项目就是这么定的，
// 交给 apierror.ClassifyHTTPStatus 兜 —— 它 default 分支就是 500，所以 service 里
// 一句 errors.New("video not found") 出来天然是 500，不需要额外机制）。
type LikeHandler struct {
	service *LikeService
}

func NewLikeHandler(service *LikeService) *LikeHandler {
	return &LikeHandler{service: service}
}

// Like POST /like/like（JWT）点赞
func (h *LikeHandler) Like(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VideoID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "video_id is required"})
		return
	}

	// 身份从 token 取、不从请求体取 —— 让客户端能传 account_id，等于把
	// "代表谁点赞"的权力交出去（LikeRequest 里根本没有这个字段）
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	if err := h.service.Like(c.Request.Context(), &Like{
		VideoID:   req.VideoID,
		AccountID: accountID,
	}); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "like success"})
}

// Unlike POST /like/unlike（JWT）取消点赞
func (h *LikeHandler) Unlike(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VideoID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "video_id is required"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	if err := h.service.Unlike(c.Request.Context(), &Like{
		VideoID:   req.VideoID,
		AccountID: accountID,
	}); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unlike success"})
}

// IsLiked POST /like/isLiked（JWT）只读，阶段7 不给它挂限流
//
// 注意返回的是 200 + `{"is_liked": false}` 的两种情况要分清：
//   - video_id 为 0（参数问题）→ 400，走不到这里
//   - video_id 合法但视频不存在 → 200 + false（查询接口幂等地回答"你赞过吗"）
func (h *LikeHandler) IsLiked(c *gin.Context) {
	var req LikeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VideoID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "video_id is required"})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	liked, err := h.service.IsLiked(c.Request.Context(), req.VideoID, accountID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"is_liked": liked})
}

// ListMyLikedVideos POST /like/listMyLikedVideos（JWT）我的点赞列表
//
// **这个接口没有请求体，所以不 bind**：gin 对空 body 调 ShouldBindJSON 会返回 EOF，
// 把"查询我自己的点赞列表"变成一个 400，说不通。身份全在 token 里，本来也没别的输入。
func (h *LikeHandler) ListMyLikedVideos(c *gin.Context) {
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	videos, err := h.service.ListLikedVideos(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	// 响应是**裸数组**（不是 {"video_list": ...}），和 /video/listByAuthorID 一致
	c.JSON(http.StatusOK, nonNilVideos(videos))
}

// nonNilVideos 把 nil 切片换成长度 0 的切片。
// nil 切片序列化成 JSON 是 null，空切片是 []，前端 v-for 遇到 null 会炸。
// 和 feed/handler.go 的 nonNilFeedVideoItems 是同一个道理、也在同一层做
// —— "JSON 形状的兜底统一留在 HTTP 层"这条纪律。
func nonNilVideos(videos []Video) []Video {
	if videos == nil {
		return []Video{}
	}
	return videos
}
