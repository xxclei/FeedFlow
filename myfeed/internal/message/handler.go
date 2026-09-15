package message

import (
	"net/http"
	"strings"

	"myfeed/internal/apierror"
	"myfeed/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

// MessageHandler 私信模块的 HTTP 层。**两个接口，都要求登录。**
//
// 这是全项目唯一一个"**没有任何公开接口**"的模块 —— like/comment/social
// 都有不鉴权的读接口（isLiked、listAll、counts），因为它们读的是公开内容。
// 私信是私有数据，**每一个字节都必须先有身份**，所以 router 里
// /message 组直接整体套 JWTAuth，没有 protected/public 的分层。
//
// 这条差别值得记：**鉴权是数据属性，不是模块风格。** 同一套四件套，
// 因为数据私有与否，路由结构就不一样。
type MessageHandler struct {
	service *MessageService
}

func NewMessageHandler(service *MessageService) *MessageHandler {
	return &MessageHandler{service: service}
}

// Send POST /message/send 发私信
//
// 请求体 `{to_id, content}` —— **没有 from_id**，发件人只从 token 取。
// 和 Follow 的 `{vlogger_id}` 是同一条纪律：让客户端能传 from_id
// 等于让任何人冒充任何人发私信。
//
// 响应是**落库后的完整 Message 对象**（带 id 和 created_at），
// 不是 `{"message":"ok"}`。这是文档明确要求的形状，也是更好的 API 设计：
// 客户端把响应当作"服务端确认版"直接插进消息列表，不需要为确认而重拉会话。
//
// ⚠ 手工 curl 时注意两件事（都踩过）：
//  1. gin **忽略未知字段**（见 memory: gin-ignores-unknown-json-fields）。
//     写成 `{"toId":2}` 或 `{"to_user_id":2}` 不会报错，只会被静默忽略，
//     然后 to_id 保持 0 → 返回 400 "to_id and content are required"。
//     **看起来像校验太严，实际是字段名写错了。** 先读结构体的 json tag。
//  2. 限流桶 `account_login` 那类是按 IP 的，这里没有限流 —— 私信**刻意不限流**
//     （原项目也没有）。真要加，桶名应该按账号而不是按 IP，理由见 ratelimit 包。
func (h *MessageHandler) Send(c *gin.Context) {
	var req SendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	// 校验顺序：先验请求体形状，再取身份。和 SocialHandler.Follow 一致。
	//
	// **content 用 TrimSpace 后判空**，不是判 `== ""`：
	// 一个全是空格的私信（`"   "`）在 UI 上看着就是条空消息，
	// 存进去之后收件人会看到一个空白气泡，且**永远删不掉**（没有删除接口）。
	// 判空要按"人眼看到的空"来判，不是按"字节数为 0"。
	//
	// 注意这里**没有长度上限**。Content 是 text，理论上能塞几十 MB ——
	// 没有 MaxBytesReader 是原项目的形态（也是全项目的形态）。
	// 想补的话在 router 上加中间件统一限，别在这里单独加。
	if req.ToID == 0 || strings.TrimSpace(req.Content) == "" {
		// 文案照文档逐字：`to_id and content are required`。
		// 两个字段共用一个错误消息（而不是分开报 "to_id is required"）——
		// 原项目如此，验收清单也是按这个字符串写的。
		c.JSON(http.StatusBadRequest, gin.H{"error": "to_id and content are required"})
		return
	}

	fromID, err := jwt.GetAccountID(c)
	if err != nil {
		// 显式 401 而不是 ClassifyHTTPStatus（后者对裸 error 兜成 500）。
		// 理由和 SocialHandler 里那段一模一样：GetAccountID 失败只可能是
		// "没身份"。而且这条路径在 JWTAuth 之后**不可达** —— 中间件早就掐断了。
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	// TrimSpace 的结果存进去，不是原文 —— 否则上面那次判空就成了摆设：
	// 挡住了 "   "，却把 "  hi  " 原样存下，前端渲染出多余的空白。
	// **校验用的值和存下来的值必须是同一个**，这是这类"清洗型校验"的通则。
	msg := &Message{
		FromID:  fromID,
		ToID:    req.ToID,
		Content: strings.TrimSpace(req.Content),
		// IsRead 不设 —— 默认 false，且本项目不写它（见 entity.go 的说明）
	}

	// 错误码映射（to_id 不存在 → gorm.ErrRecordNotFound → **404**）：
	// 这是本项目对原项目的改进点（原项目不校验，返回 200 + 一条孤儿数据）。
	// 前端拿到 404 会显示 "用户不存在"，比"发送成功但对方收不到"好得多。
	if err := h.service.Send(c.Request.Context(), msg); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	// 序列化 msg 本身。此刻它已经被 GORM 回填了 ID 和 CreatedAt，
	// 所以响应里 id 非 0 —— **验收时要检查这个**（id=0 说明传值而不是传指针，
	// 或者 Create 之后没回填，那是静默的半成品）。
	c.JSON(http.StatusOK, msg)
}

// List POST /message/list 拉我和某人的双向会话
//
// 请求体 `{peer_id}`，返回 `{"messages":[...]}`，最近 50 条，created_at **倒序**
// （最新的在前）。前端渲染聊天窗时需要再反转一次成"从上到下由旧到新"。
//
// ---------- 关于 `{"messages":[]}` 而不是 `null` ----------
//
// 这一行兜底（下面那个 if msgs == nil）是**必须的**，不是洁癖：
// repo.List 用 `var msgs []Message` 声明，GORM 空结果时不 append，
// 于是 msgs 保持 nil → `json.Marshal(nil slice)` → 字面量 `null`。
// 前端 `resp.messages.map(...)` 在 null 上直接 TypeError。
//
// 这个坑在本项目已经踩过三轮（social 的 followers、like 的 videos、
// comment 的 listAll），**第四次照抄兜底**。它也解释了为什么验收清单里
// 专门有一条"空会话 → `{"messages":[]}` 而非 null"—— 那不是格式偏好，
// 是"前端会不会崩"。
//
// 结构性观察：这个兜底本来可以写在 repo 里（`return []Message{}, nil`），
// 放 handler 的理由是**序列化是 handler 的职责** —— repo 的调用方可能
// 只是要 len()，那 nil 和空切片完全等价，不该让 repo 为 JSON 的癖好买单。
func (h *MessageHandler) List(c *gin.Context) {
	var req ListMessagesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	// peer_id == 0 显式挡掉（原项目没有这一步）。
	// 不挡也不会读到别人的数据（查询是 `(from=me and to=0) or (from=0 and to=me)`，
	// 后者恒为空，因为 to_id 有 not null 约束、没有 id=0 的账号）——
	// 挡它是为了让"忘了传参数"和"没聊过"给出**不同的信号**：
	// 前者 400，后者 200 + 空数组。否则调试时两者长得一模一样。
	if req.PeerID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "peer_id is required"})
		return
	}

	selfID, err := jwt.GetAccountID(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	// 注意这里**不需要任何越权检查** —— 这个接口没有"读别人的会话"这个选项。
	// self 来自 token，peer 只能是"另一个人"，查询形状天然把范围锁死在
	// "我和他"之间。这比"先查再判断 owner"少一整类漏洞。
	//
	// 对照 notification 的 markRead：那里必须带 `recipient_id = ?` 条件，
	// 因为它接的是**记录 id**（客户端可以传任意 id），必须自己证明归属。
	// **能不能省掉越权检查，取决于接口收的是"关系"还是"记录 id"。**
	msgs, err := h.service.List(c.Request.Context(), selfID, req.PeerID)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	// nil → 空切片（理由见上面那段）
	if msgs == nil {
		msgs = []Message{}
	}
	c.JSON(http.StatusOK, ListMessagesResponse{Messages: msgs})
}
