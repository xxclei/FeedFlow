package video

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"myfeed/internal/account"
	"myfeed/internal/apierror"
	"myfeed/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

// VideoHandler 文件上传 + 参数绑定 + 状态码，业务在 service
type VideoHandler struct {
	service        *VideoService
	accountService *account.AccountService // 原项目如此：实际未使用（死依赖），保留签名对齐
}

func NewVideoHandler(service *VideoService, accountService *account.AccountService) *VideoHandler {
	return &VideoHandler{service: service, accountService: accountService}
}

// PublishVideo POST /video/publish（JWT）
func (vh *VideoHandler) PublishVideo(c *gin.Context) {
	var req PublishVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	username, err := jwt.GetUsername(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	video := &Video{
		AuthorID:    authorId,
		Username:    username, // 写时冗余：从 JWT claims 抄，Feed 列表不用回查 accounts 表
		Title:       req.Title,
		Description: req.Description,
		PlayURL:     req.PlayURL,
		CoverURL:    req.CoverURL,
		CreateTime:  time.Now(),
	}
	if err := vh.service.Publish(c.Request.Context(), video, req.TagNames); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, video)
}

// UploadVideo POST /video/uploadVideo（JWT，multipart，字段名 file）
func (vh *VideoHandler) UploadVideo(c *gin.Context) {
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		return
	}

	// 上限和分片 init 共用同一个常量（chunk_entity.go），改一处两边都跟着变
	if f.Size <= 0 || f.Size > maxUploadSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		return
	}

	ext := strings.ToLower(filepath.Ext(f.Filename))
	if ext != ".mp4" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .mp4 is allowed"})
		return
	}

	// 目录结构：.run/uploads/videos/<作者ID>/<日期>/ —— 按日期分目录，
	// 避免单目录塞几十万文件拖垮文件系统
	date := time.Now().Format("20060102")
	relDir := filepath.Join("videos", fmt.Sprintf("%d", authorId), date)
	root := filepath.Join(".run", "uploads")
	absDir := filepath.Join(root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filename, err := randHex(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		return
	}
	filename = filename + ext
	absPath := filepath.Join(absDir, filename)

	if err := c.SaveUploadedFile(f, absPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	urlPath := path.Join("/static", "videos", fmt.Sprintf("%d", authorId), date, filename)

	c.JSON(http.StatusOK, gin.H{
		"url":      buildAbsoluteURL(c, urlPath),
		"play_url": buildAbsoluteURL(c, urlPath),
	})
}

// UploadCover POST /video/uploadCover（JWT，multipart，字段名 file）
func (vh *VideoHandler) UploadCover(c *gin.Context) {
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	f, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing file"})
		return
	}

	const maxSize = 10 << 20 // 10MB
	if f.Size <= 0 || f.Size > maxSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file size"})
		return
	}

	ext := strings.ToLower(filepath.Ext(f.Filename))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "only .jpg/.jpeg/.png/.webp is allowed"})
		return
	}

	date := time.Now().Format("20060102")
	relDir := filepath.Join("covers", fmt.Sprintf("%d", authorId), date)
	root := filepath.Join(".run", "uploads")
	absDir := filepath.Join(root, relDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filename, err := randHex(16)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate filename"})
		return
	}
	filename = filename + ext
	absPath := filepath.Join(absDir, filename)

	if err := c.SaveUploadedFile(f, absPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	urlPath := path.Join("/static", "covers", fmt.Sprintf("%d", authorId), date, filename)

	c.JSON(http.StatusOK, gin.H{
		"url":       buildAbsoluteURL(c, urlPath),
		"cover_url": buildAbsoluteURL(c, urlPath),
	})
}

// DeleteVideo 删除单条视频（JWT，路由 /video/delete）
//
// 这里原来挂着一句"原项目有实现但路由里未挂载——死代码，保持对齐"，是错的：
// router.go 的 protectedVideoGroup 里一直挂着 /delete，这是**活代码**。
// 顺带说清它与 DeleteVideosBatch 的分工：单条走这条（能报 404），
// 批量走下面那条（只能报"跳过"），两者共用同一个删除实现。
func (vh *VideoHandler) DeleteVideo(c *gin.Context) {
	var req DeleteVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if err := vh.service.Delete(c.Request.Context(), req.ID, authorId); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "video deleted"})
}

// DeleteVideosBatch POST /video/deleteBatch（JWT）
//
// 批量删除。返回 {deleted, deleted_ids, skipped_ids} —— 语义是**部分成功**，
// 不属于自己的、以及已经不存在的 id 不报错，而是进 skipped_ids 如实回报。
// 完整的理由见 entity.go 里 DeleteBatchResponse 的注释。
//
// ---------- 为什么这里的 400 是字面量，不走 apierror.ClassifyHTTPStatus ----------
//
// 本文件其它 handler 对 ShouldBindJSON 的失败统一写 ClassifyHTTPStatus(err)，
// 而绑定错误**不是** apierror，于是落到默认分支变成 **500**：
// 往 /video/publish 发一个 `{"title": 123}` 会得到"服务器内部错误"。
// 那是一处既有的不一致（feed/handler.go 里后来加的校验一律用字面量 400）。
// 新 handler 不复制它 —— 参数形状错误就是 400，不该让客户端以为是自己把服务打挂了。
//
// 也不引入 apierror.ErrValidation：它存在但全项目 0 使用，为一个 handler
// 新开一条没人走过的错误路径不划算（而且它是 errors.New("validation error") 这种
// 固定文案，直接用会把真实原因吞掉，必须 fmt.Errorf("%w") 包一层才行）。
func (vh *VideoHandler) DeleteVideosBatch(c *gin.Context) {
	var req DeleteBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	authorId, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	// service 里还会再 dedupeIDs 一次。这里先算，是为了让**上限判断的对象**
	// 和真正要处理的集合一致：[5,5,5,...] 重复 200 次实际上只请求了 1 条，
	// 按原始长度判会把它误拒成"一次删太多"
	ids := dedupeIDs(req.IDs)
	if len(ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids is required"})
		return
	}
	// 封顶的理由是请求体大小 + 事务持有时间，不是"业务上不该一次删这么多"
	if len(ids) > maxBatchDeleteIDs {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many ids (max %d)", maxBatchDeleteIDs)})
		return
	}

	resp, err := vh.service.DeleteBatch(c.Request.Context(), ids, authorId)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, resp)
}

// ListByAuthorID POST /video/listByAuthorID（公开）
func (vh *VideoHandler) ListByAuthorID(c *gin.Context) {
	var req ListByAuthorIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	videos, err := vh.service.ListByAuthorID(c.Request.Context(), req.AuthorID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if videos == nil {
		videos = []Video{} // 前端契约：空列表返回 [] 而不是 null
	}
	c.JSON(200, videos)
}

// GetDetail POST /video/getDetail（公开）
func (vh *VideoHandler) GetDetail(c *gin.Context) {
	var req GetDetailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	video, err := vh.service.GetDetail(c.Request.Context(), req.ID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, video)
}

// UpdateLikesCount POST 用指定值覆盖点赞数（阶段4点赞链路对接用）
func (vh *VideoHandler) UpdateLikesCount(c *gin.Context) {
	var req UpdateLikesCountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if err := vh.service.UpdateLikesCount(c.Request.Context(), req.ID, req.LikesCount); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "likes count updated"})
}

// randHex 生成 n 字节随机数的 hex 字符串（文件名防猜测/防覆盖）
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// buildAbsoluteURL 拼 http://host/static/... 的可访问地址
func buildAbsoluteURL(c *gin.Context, p string) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if xf := c.GetHeader("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	return fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, p)
}
