// Package profile 跨模块聚合（阶段6）：/account/getProfile。
//
// # 为什么它自成一个包，而不是一个方法
//
// 这个接口要同时读三个模块的数据：
//
//	account —— 账号信息（走 accountService.FindByID）
//	video   —— video_count / total_likes（直接调 repo）
//	social  —— follower_count / vlogger_count（直接调 repo）
//
// 文档给了三条路：① router 里内联匿名函数（原项目的选择）② 建独立的聚合层
// ③ 挪进 accountHandler。**③ 在本项目里是编译不过的** ——
//
//	internal/video 已经 import 了 internal/account
//	  （comment_service.go 用 account.AccountRepository 查 @提及的用户、
//	   video_handler.go 用 account.AccountService 补 username）
//	所以 internal/account 一旦反过来 import internal/video，就是**循环依赖**。
//
// 这不是"不喜欢这个方案"，是 Go 不允许。文档写 ③ 的时候没注意这条依赖边 ——
// 值得记住：**依赖方向是架构的硬约束，不是风格偏好**。account 是全项目最底层的模块，
// 它一旦开始认识 video/social，整个依赖图就塌了。
//
// 剩下 ① 和 ②。选 ② 的理由：
//
//	router.go 已经是 150 行**纯装配**代码（new repo → new service → 挂路由）。
//	往里面塞业务逻辑，这段代码就再也没有办法被单独测试 ——
//	要测 getProfile 的错误策略，得先起一个完整的 gin + MySQL。
//	而聚合逻辑恰恰是"错误策略不对称"这种最需要被测的东西。
//
// # 这一层为什么不遵循"handler 只认识 service"
//
// 因为它**本来就是胶水**。四个数据源里三个是"要实时新鲜的统计数"，
// 原项目有意绕过 service 直接调 repo —— service 层会带来缓存/额外校验的假想，
// 而这里要的就是"一个 COUNT 打到底"。所以本包只有 handler，没有 service，
// 依赖既有 service（account，因为它带缓存逻辑）也有 repo（video/social）。
//
// 一个包、一个文件、一个 handler，看着不成比例 —— 但它把一个真实的架构问题
// 摆在了明处：**没有任何单一模块该为"聚合读"负责**，所以只能给它一个自己的位置。
package profile

import (
	"log"
	"net/http"

	"myfeed/internal/account"
	"myfeed/internal/apierror"
	"myfeed/internal/social"
	"myfeed/internal/video"

	"github.com/gin-gonic/gin"
)

// ProfileHandler 聚合三个模块的 handler。
type ProfileHandler struct {
	accountService *account.AccountService
	videoRepo      *video.VideoRepository
	socialRepo     *social.SocialRepository
}

func NewProfileHandler(
	accountService *account.AccountService,
	videoRepo *video.VideoRepository,
	socialRepo *social.SocialRepository,
) *ProfileHandler {
	return &ProfileHandler{
		accountService: accountService,
		videoRepo:      videoRepo,
		socialRepo:     socialRepo,
	}
}

// GetProfile POST /account/getProfile（**公开，无鉴权**）
//
// 请求 `{"account_id": 2}`，响应：
//
//	{"account": {"id":2,"username":"bob","avatar_url":"...","bio":"..."},
//	 "video_count": 12, "total_likes": 345,
//	 "follower_count": 7, "vlogger_count": 3}
//
// 挂在 accountGroup 上（公开）：看别人的主页不需要登录，和 /account/findByID 同类。
//
// # 错误策略是**不对称**的，这是本接口最值得看的一处
//
//	账号不存在        → 拒绝（整个请求没意义，四个统计数不知道该算谁的）
//	四个统计数出错    → 全部吞掉，按 0 返回
//
// 为什么统计数可以降级：这是**展示数据**。粉丝数那次 COUNT 超时了，
// 与其给用户一个 500 页面，不如给他主页 + 一个 0（虽然不准，但页面能用）。
// 主页的骨架（头像、名字、简介）和统计数是完全不同等级的数据。
//
// 原项目连日志都不打（`videoCount, _ := ...`），结果线上看到"计数显示 0"
// 时完全无从下手。这里补上 log.Printf —— 这是文档点名建议的改进。
func (h *ProfileHandler) GetProfile(c *gin.Context) {
	var req account.GetProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}
	if req.AccountID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id is required"})
		return
	}

	// 主数据：走 service（它可能带缓存/别的逻辑），失败就整个拒绝。
	//
	// **这里保持原项目的显式 500，不走 ClassifyHTTPStatus**。
	// 走 ClassifyHTTPStatus 的话 gorm.ErrRecordNotFound 会被翻译成 **404** ——
	// 那其实更对（"这个用户不存在"是客户端问题，不是服务端故障），
	// 一个公开接口因为参数里塞了个不存在的 ID 就吐 500，会污染监控。
	//
	// 想改的话就一行：把下面这两句换成
	//   c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()})
	// 保留原样是为了对齐（阶段5 的"全空格 content → 500"是同一类"照抄瑕疵"）。
	acc, err := h.accountService.FindByID(c.Request.Context(), req.AccountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	// 四个统计数：直接调各模块的 repo，**绕过 service**。
	// 这是有意的取舍 —— 这些数字要实时新鲜，不值得缓存，也就用不上 service 那层。
	//
	// 阶段2 就备好的两个（CountByAuthor / TotalLikesByAuthor）在这里第一次被调用。
	// TotalLikesByAuthor 内部是 COALESCE(SUM(likes_count), 0)：没有视频时 SUM 返回
	// SQL NULL，不兜会 Scan 失败或得到 0 以外的怪值。
	videoCount, err := h.videoRepo.CountByAuthor(ctx, req.AccountID)
	if err != nil {
		log.Printf("getProfile: CountByAuthor 失败: accountID=%d, err=%v", req.AccountID, err)
	}
	totalLikes, err := h.videoRepo.TotalLikesByAuthor(ctx, req.AccountID)
	if err != nil {
		log.Printf("getProfile: TotalLikesByAuthor 失败: accountID=%d, err=%v", req.AccountID, err)
	}
	followerCount, err := h.socialRepo.CountFollowers(ctx, req.AccountID)
	if err != nil {
		log.Printf("getProfile: CountFollowers 失败: accountID=%d, err=%v", req.AccountID, err)
	}
	vloggerCount, err := h.socialRepo.CountVloggers(ctx, req.AccountID)
	if err != nil {
		log.Printf("getProfile: CountVloggers 失败: accountID=%d, err=%v", req.AccountID, err)
	}

	// 四个 COUNT 是四次独立的查询，**没有跨表快照**：别人正好在这中间关注/发视频，
	// 数字之间就可能差一拍。展示数据，接受。
	//
	// 响应里显式构造 FindByIDResponse 而不是把 acc（*account.Account）直接塞进去：
	// 一是响应契约就是这四个字段（阶段1 敲好的），二是 Account 上带着
	// Password/Token/RefreshToken —— 它们有 json:"-" 不会泄露，但"靠标签兜底"
	// 不如"根本不进结构体"稳。
	c.JSON(http.StatusOK, account.GetProfileResponse{
		Account: account.FindByIDResponse{
			ID:        acc.ID,
			Username:  acc.Username,
			AvatarURL: acc.AvatarURL,
			Bio:       acc.Bio,
		},
		VideoCount:    videoCount,
		TotalLikes:    totalLikes,
		FollowerCount: followerCount,
		VloggerCount:  vloggerCount,
	})
}
