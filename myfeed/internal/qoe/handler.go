package qoe

import (
	"net/http"

	"myfeed/internal/apierror"
	"myfeed/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
)

// QoEHandler 埋点模块的 HTTP 层。两个接口，**都是公开的**。
//
// ---------- 为什么 report 挂 SoftJWTAuth 而不是 JWTAuth ----------
//
// 这和 message 模块形成最鲜明的对照 —— 那边是全项目唯一一个
// "每个字节都必须先有身份"的模块（私信是私有数据）。埋点正相反：
//
//	游客的播放数据**和登录用户一样有价值**，甚至更有价值。
//
// 因为游客是最容易因为卡顿直接流失的那群人 —— 他们没登录、没有沉没成本，
// 卡一次就关掉再也不回来。只看登录用户的数据会得到一个
// **系统性偏乐观**的结论：留下的人本来就是"能忍的"那群。
//
// 换成 JWTAuth 的后果很隐蔽：接口不会报错，只是游客的那一半数据
// 静默地变成 401 然后被前端吞掉，看板上的数字全部偏高。
// **这类"少了一半样本但不报错"的过滤，是埋点系统最常见的死法。**
//
// ---------- stats 为什么也公开 ----------
//
// 它只返回聚合分布（首帧分位、卡顿率、码率分桶），不含任何用户维度信息 ——
// 没有任何一行数据能追到某个人。和 /video/getDetail 是同一类：
// 公开内容 + 公开聚合，游客也该能看。
//
// 真要担心的是**信息泄露的边角**：单条视频的 stats 会暴露"这条视频有多少人看过"。
// 但那件事 /video/getDetail 的 likes_count 早就暴露了，不是新边界。
type QoEHandler struct {
	service *QoEService
}

func NewQoEHandler(service *QoEService) *QoEHandler {
	return &QoEHandler{service: service}
}

// Report POST /qoe/report 上报一次播放会话。
//
// 调用方是前端的 useQoE.ts，三条刷出路径（组件卸载 / 页面隐藏 / 页面卸载）
// 都打这个接口。**同一 session_id 会被上报多次**，服务端是 upsert ——
// 见 repo.Report 顶上那段，那是本模块唯一需要理解的设计。
//
// 响应返回落库后的 session 快照。**它有点特殊：返回值是"可选的"。**
// 三条刷出路径里有两条（pagehide / visibilitychange）根本读不到响应 ——
// 但正常路径（组件卸载走 fetch）能读到，那时它有真实的调试价值：
// 客户端可以对照"我报的"和"服务端存的"确认清洗没削掉东西
// （比如发现服务端把自己报的 startup_ms 从 120000 削成了 60000，
// 就知道客户端的计时器出问题了）。
//
// ⚠ **唯一一个不能这样对照的字段是 created_at。** 它在响应里是 GORM
// 填在内存结构体上的"本次请求时刻"，而库里存的是**首次**写入的时刻 ——
// upsert 的 DoUpdates 里没有 created_at（见 repo.Report），所以重复上报
// 不会把它刷新。也就是说：第一次上报时两者一致，之后响应里的 created_at
// 会比库里的新。
//
// 这是**有意的语义**而不是 bug：created_at = 这次播放**开始**的时刻，
// 正好是 stats 时间窗该用的口径（"这段时间里开始的播放表现如何"），
// 也正好是 idx_qoe_video_time 想要的排序键。要的是它稳定，不是它最新。
// 但拿响应里的值当"落库结果"用就会错，所以在这里点名。
// 其余数字字段没有这个问题：它们都在 DoUpdates 列表里，
// 回显的就是真正写进去的那份（已被 service 层夹取过的值）。
func (h *QoEHandler) Report(c *gin.Context) {
	var req ReportQoERequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	// session_id 是**唯一一个拒而不是削**的字段：它是 upsert 的键，
	// 削掉/截断它会改变"这是哪次播放"的语义 ——
	// 截断后两次不同播放可能撞成同一次（静默的数据丢失），
	// 而空值会让所有上报互相覆盖。
	//
	// 对比 service.go 里的数字字段：那些削（能修的就修），
	// 这里拒（修不了的才拒）。判据是**削之后语义还成不成立**。
	if req.SessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
		return
	}
	if len(req.SessionID) > maxSessionIDLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id too long"})
		return
	}

	// video_id 为 0 直接拒 —— 但理由**不是**"要校验视频存在"（那是刻意不做的，
	// 见 service.Report 顶上那三条）。理由是 video_id 缺了，
	// 这条数据就永远只能进"总体"、进不了任何一条视频的画像，
	// 而"某条视频播得怎么样"恰恰是这个模块最主要的用途。
	// **这不是完整性校验，是"这条数据有没有用"的判断。**
	if req.VideoID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "video_id is required"})
		return
	}

	// 软鉴权下"没带 token"是正常路径，不是错误 —— GetAccountID 会返回 error，
	// 这里把它**降级成 0（游客）**而不是 401。和 feed 里
	// "viewerAccountID 留空 → handler 里降级成 0"是同一条处理。
	//
	// ⚠ 注意：SoftJWTAuth 对"带了 token 但无效"仍然是 401 掐断，
	// 所以走到这里的 error **只可能**是"没带"。这里不区分两者 ——
	// 区分了也没用，因为带了无效 token 的请求根本到不了这一行。
	accountID, err := jwt.GetAccountID(c)
	if err != nil {
		accountID = 0
	}

	// UA 从请求头取，**不用请求体里的**。客户端自报的 UA 是一个可以随便写的
	// 字符串，没有任何排查价值；而真实 UA 是排查"只有 Safari 卡顿"这类
	// 问题时唯一可信的线索。
	event, err := h.service.Report(c.Request.Context(), &req, accountID, c.Request.UserAgent())
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, event)
}

// Stats POST /qoe/stats 聚合看板。
//
// 请求体 `{video_id, days}`，两个都可省：
//
//	video_id = 0 → 全部视频（默认）
//	days     = 0 → 不限时间；**但这里会把 0 改成默认 7 天**，见下
//
// ⚠ 一个刻意的例外：**"不限时间"没法从请求表达**。days=0 被解释成
// "用默认值 7"。想查全时段只能传负数。
//
// 为什么这么设计：这是"默认值"和"哨兵值"撞车的经典场景 ——
// Go 的零值 0 既表示"没传"也表示"我就要 0"。三种解法里：
//
//	用指针 *int    → 能区分，但请求体变丑，且 gin 的 bind 对指针不友好
//	另加一个 bool  → 能区分，但多一个字段，且两个字段可能自相矛盾
//	**让 0 表示默认，负数表示不限** → 零值即默认，符合直觉，且不会误用
//
// 选了第三种。理由是**看板的默认行为应该是"看最近 7 天"**：
// 播放侧的任何改动（缓存头、ABR、降级）都会改变 QoE，
// 跨改动的全时段均值必然混合了两个世界的数字，是个假数。
// 想看全时段是**主动的、少见的**动作，用负数表达它很合适。
func (h *QoEHandler) Stats(c *gin.Context) {
	var req StatsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	days := req.Days
	if days == 0 {
		days = defaultStatsDays
	}

	resp, err := h.service.Stats(c.Request.Context(), req.VideoID, days)
	if err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// defaultStatsDays 看板默认回看 7 天。理由见上面 Stats 的注释 ——
// 核心是"数字必须对应现在这份代码"，跨改动的均值没有意义。
const defaultStatsDays = 7
