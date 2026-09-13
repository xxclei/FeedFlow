package video

import (
	"net/http"

	"myfeed/internal/account"
	"myfeed/internal/apierror"
	"myfeed/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

// CommentHandler 评论模块的 HTTP 层。
//
// **注意第二个依赖：accountService —— 这是全项目 handler 跨模块依赖 service 的唯一样板。**
//
// 为什么要有它：Comment.Username 是**写时冗余**（见 comment_entity.go），
// 发布时必须把"token 里的 accountID"翻译成"username 字符串"存进评论行。
// 这个翻译动作被放在 handler，而不是 service，理由是原项目的分层定义：
//
//	service 只收"完整的业务对象"；
//	"身份 → 展示信息"的装配属于**请求组装**，是 HTTP 层的活。
//
// 于是 service 的 Publish 签名干干净净（收一个填好的 *Comment），
// 它完全不知道 username 是查库来的还是从别处来的 —— 阶段9 的 MQ Worker
// 从消息里读 username 直接填，同一个 Publish 一行不改。
//
// 依赖的是**读查询**（FindByID），不引入写路径耦合：评论的写路径上
// 没有任何一行会去改 accounts 表。这是"跨模块依赖可以，但要有方向"的示范。
//
// 顺带回答一个必然会冒出来的问题：JWT claims 里明明就有 username
// （jwt 中间件 c.Set("username")），为什么不省这一次查库？
//
//	① claims 里的是**签发时刻**的快照 —— 用户改名后，旧 token 里的名字是旧的，
//	   而 FindByID 拿的是当前值。（和 videos.username / comments.username
//	   是同一类"快照 vs 现值"的问题，但那两处是我们主动选的快照，
//	   这里没有任何理由主动选一个会过期的值。）
//	② 查这一次顺带确认账号**仍然存在** —— 被删除的账号不该还能发评论。
type CommentHandler struct {
	service        *CommentService
	accountService *account.AccountService
}

func NewCommentHandler(service *CommentService, accountService *account.AccountService) *CommentHandler {
	return &CommentHandler{service: service, accountService: accountService}
}

// PublishComment POST /comment/publish（JWT）发表评论
func (h *CommentHandler) PublishComment(c *gin.Context) {
	var req PublishCommentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	// 两个参数问题就地修成 400，不进 service。
	// 注意这里查的是 `== ""` 而不是 TrimSpace —— "全是空格"会穿透到
	// service 被拦下，报的是 **500**（原项目瑕疵，详见 comment_service.go 里的说明）。
	if req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content is required"})
		return
	}
	if req.VideoID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "video_id is required"})
		return
	}

	// 身份只从 token 取。PublishCommentRequest 里**没有** author_id 字段 ——
	// 让客户端能传作者 ID，等于把"代表谁发言"的权力交出去。
	authorID, err := jwt.GetAccountID(c)
	if err != nil {
		// jwt.GetAccountID 返回的是裸 error，ClassifyHTTPStatus 会兜成 500。
		// 严格说这里该是 401，但这条路径在 JWTAuth 之后**不可达**
		// （中间件没拿到 accountID 就已经掐断了），所以保持和 like_handler 一致。
		// 阶段6 的 social handler 会在同样位置显式写 401 —— 那两处也是不可达的，
		// 纯粹是"照文档写的"。知道这个区别就行，不用强行统一。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	// 写时冗余的那一次查库（本文件头上那段注释的主角）
	user, err := h.accountService.FindByID(c.Request.Context(), authorID)
	if err != nil {
		// 账号不存在 → repo 返回 gorm.ErrRecordNotFound → 翻译成 **404**。
		// 这是本项目里少数几个真能走到 404 的业务分支之一
		// （ClassifyHTTPStatus 只认 gorm.ErrRecordNotFound）。
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	if err := h.service.Publish(c.Request.Context(), &Comment{
		Username: user.Username, // ← 本次请求里唯一一次"身份 → 展示名"的翻译
		VideoID:  req.VideoID,
		AuthorID: authorID,
		Content:  req.Content,
	}); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "comment published successfully"})
}

// DeleteComment POST /comment/delete（JWT）删除评论
//
// 请求体里**只有 comment_id**。权限判断所需的身份从 token 取，
// 判断逻辑在 service.Delete（不能下放给异步方，见那里的注释）。
func (h *CommentHandler) DeleteComment(c *gin.Context) {
	var req DeleteCommentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	if req.CommentID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "comment_id is required"})
		return
	}

	if err := h.service.Delete(c.Request.Context(), req.CommentID, accountID); err != nil {
		// 三个错误在这里各走各的状态码，验收时要能逐个对上：
		//   comment not found  → 裸 error          → 500（原项目如此，不是 404）
		//   apierror.ErrForbidden → 403（本项目改进，原项目是 401）
		//   其它               → 500
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "comment deleted successfully"})
}

// GetAllComments POST /comment/listAll（公开，不挂 JWT）
//
// 公开的理由：看视频评论不需要登录。响应是**裸数组**（不是 {"comments": [...]}），
// 和 /video/listByAuthorID、/like/listMyLikedVideos 的形状一致。
func (h *CommentHandler) GetAllComments(c *gin.Context) {
	var req GetAllCommentsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.VideoID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "video_id is required"})
		return
	}

	comments, err := h.service.GetAll(c.Request.Context(), req.VideoID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, nonNilComments(comments))
}

// nonNilComments 把 nil 切片换成长度 0 的切片 —— 和 feed 的 nonNilFeedVideoItems、
// like 的 nonNilVideos 是同一条纪律、同一层：JSON 形状的兜底统一留在 HTTP 层。
//
// 这里具体防的是：没有评论的视频，GORM 的 Find 留下 nil 切片，
// 序列化成 `null`，前端 `comments.length` 直接 TypeError。
// 原项目是在 handler 里写 `if comments == nil { comments = []Comment{} }`，
// 效果一样，抽成函数只是让"这是本层的纪律"这件事看得见。
func nonNilComments(comments []Comment) []Comment {
	if comments == nil {
		return []Comment{}
	}
	return comments
}
