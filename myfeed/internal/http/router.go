package http

import (
	"log"
	"time"

	"myfeed/internal/account"
	"myfeed/internal/config"
	"myfeed/internal/feed"
	"myfeed/internal/message"
	jwt "myfeed/internal/middleware/jwt"
	rabbitmq "myfeed/internal/middleware/rabbitmq"
	"myfeed/internal/middleware/ratelimit"
	rediscache "myfeed/internal/middleware/redis"
	"myfeed/internal/notification"
	"myfeed/internal/profile"
	"myfeed/internal/qoe"
	"myfeed/internal/search"
	"myfeed/internal/social"
	"myfeed/internal/video"
	"myfeed/internal/worker"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 阶段9：API 进程要消费的三条队列名。
//
// ⚠ 又是**手写的字面量**，和 cmd/worker/main.go 里那几个、以及 rabbitmq 包内
// 的常量是三个地方。这是刻意的重复（那些常量是 unexported，main 包 import 不到），
// 但这次的风险**比 cmd/worker 那边高一档**：
//
//	cmd/worker 里写错 → Consume 不存在的队列 → 启动时就 404，立刻炸
//	这里写错        → 声明了一条**空绑定**的新队列 → **不报错**
//	                  （RabbitMQ 允许创建没有消息会进来的队列）
//	                  → 表现是"通知一条都不来"，而且队列看着一切正常
//
// 唯一的真实防线是 topology.go 里 notificationLinks 用的那三个常量 ——
// **声明用常量、消费用字面量**，如果哪天有人在 topology.go 里改了名字，
// 这里就会静默错开。想让它们不可能漂移，唯一办法是把常量导出，
// 但那会把"队列名"这个实现细节暴露成 rabbitmq 包的公开 API。
// 本项目选择保留重复 + 这条注释：**知道这里有缝，比假装没有强。**
const (
	timelineQueueName = "video.timeline.update.queue"

	notificationLikeQueueName    = "notification.like"
	notificationCommentQueueName = "notification.comment"
	notificationSocialQueueName  = "notification.social"
)

// SetRouter 依赖注入大汇合：new Repo → new Service → new Handler → 挂路由。
// 每完成一个模块，就在这里加一段组装 + 一组路由。
//
// 本轮签名从 `(db)` 变成 `(db, cfg)`：**第一次有"参数"要往下传**。
// 此前所有依赖都是 db 派生出来的（repo 都是 NewXxxRepository(db)），
// 检索参数（depth / rrf_k）却是纯配置，没有 db 可以派生 ——
// 于是 router 不得不开始认识 config.Config。
// 传整个 Config 而不是只传 cfg.Search：这一层是组装点，将来会有更多模块
// 需要自己的配置段，每加一段就改一次签名反而更吵。
// 阶段7：签名加 cache *rediscache.Client。它**可以为 nil**（启动降级），
// 所以这里不做任何判空 —— nil 直接往下传，封装层的 nil 接收器保护
// 和各 service 的 `if cache != nil` 会处理它。
// 阶段9：签名加 rmq *rabbitmq.RabbitMQ。它**也可以为 nil**（启动降级，
// 和 cache 一个套路：连不上就置 nil，服务照常启动）。
//
// 注意 nil 是分两层传下去的：这里传的是 nil，下面每个
// `rabbitmq.NewXxxMQ(rmq)` 会先失败返回 nil，再传给 service。
// 所以 service 里判空判的是**封装**而不是连接 —— 那个判空同时覆盖了
// "MQ 连不上"和"这个队列声明失败"两种情况，一个 if 管两件事。
func SetRouter(db *gorm.DB, cfg config.Config, cache *rediscache.Client, rmq *rabbitmq.RabbitMQ) *gin.Engine {
	r := gin.Default()
	if err := r.SetTrustedProxies(nil); err != nil {
		log.Printf("SetTrustedProxies failed: %v", err)
	}

	// 健康检查（部署时探活用，阶段10 的 healthcheck 会调它）
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// 静态文件服务：上传的头像/视频/封面都落在 UploadRoot 下
	// URL 形如 /static/avatars/1/abcd1234.png
	//
	// ⚠ 挂载点必须和**写入侧**同一个来源（cfg.Storage）。这个字符串曾经是
	// 硬编码的 "./.run/uploads"，而门禁探测改为读配置后，两边就有了分叉的可能 ——
	// 分叉的表现不是报错，是门禁对**每一条**视频都"探测失败 → 直传"。
	// 见 config/paths.go 顶部。
	//
	// ⚠ 阶段 C **已经替换掉**了原来的 `r.Static("/static", cfg.Storage.Root())`：
	// gin 的 Static 给所有文件同一套头，而这条链路上同时需要两种相反的缓存策略
	// （.m3u8 必须 no-cache、.ts 必须 immutable）。换的实现和它接手的四件事
	// （穿越防护 / Range / ETag / MIME）在 static.go 开头。
	mountStatic(r, cfg.Storage)

	// ---------------- 限流器（阶段7 第五步）----------------
	//
	// 六个限流器集中在这里定义，是为了让整套策略**一眼可见**。
	// 分散到各自模块段里更"就近"，但也更容易漏掉某一条、或者两条用了
	// 同一个 keyPrefix（那就是两套策略共用计数器，两边都会莫名其妙地提前触发）。
	//
	//	keyPrefix           上限  窗口    维度    挂在
	//	account_register    5    1h      IP      /account/register
	//	account_login      10    1min    IP      /account/login
	//	like_write         30    1min   账号     /like/like、/like/unlike
	//	comment_write      10    1min   账号     /comment/publish、/comment/delete
	//	social_write       20    1min   账号     /social/follow、/social/unfollow
	//	qoe_report        120    1min    IP      /qoe/report
	//
	// 分界线很清楚：**注册和登录按 IP，其余写接口按账号**。
	// 原因是这两个接口正是"无限造账号/无限撞库"的入口 ——
	// 那时候还没有可信的账号身份，按账号限流等于没限（换个账号就绕过了）。
	// 到了点赞/评论/关注，人已经登录了，账号才是稳定且不可绕过的维度。
	//
	// ⚠ qoe_report 是**第三条路**：按 IP，但理由和注册登录完全相反。
	// 那两条按 IP 是因为"还没有身份"，它按 IP 是因为"**可能永远没有身份**"
	// （游客也会上报）。判据不是"接口重不重要"，而是
	// **"这个接口的调用者有没有一个稳定且不可绕过的维度可用"** ——
	// 点赞有（账号），注册没有（所以退到 IP），埋点也没有（游客没账号）。
	//
	// 读接口一律不挂：限流是为了保护写入侧（事务、通知、MQ），
	// 读接口有缓存扛着，掐读接口只会误伤正常用户。
	//
	// 注意这些 limiter 变量是在 SetRouter 里构造的**闭包**，每个请求复用同一个
	// gin.HandlerFunc —— 这没问题，因为限流的全部状态都在 Redis 里，
	// 闭包本身不持有任何请求相关数据。
	registerLimiter := ratelimit.Limit(cache, "account_register", 5, time.Hour, ratelimit.KeyByIP)
	loginLimiter := ratelimit.Limit(cache, "account_login", 10, time.Minute, ratelimit.KeyByIP)
	likeLimiter := ratelimit.Limit(cache, "like_write", 30, time.Minute, ratelimit.KeyByAccount)
	commentLimiter := ratelimit.Limit(cache, "comment_write", 10, time.Minute, ratelimit.KeyByAccount)
	socialLimiter := ratelimit.Limit(cache, "social_write", 20, time.Minute, ratelimit.KeyByAccount)

	// qoe_report 和别处**长得不一样**，值得说清为什么它按 IP 而不是按账号，
	// 以及为什么上限是 120 这么宽：
	//
	//  按 IP：这个接口**游客也会打**，而游客没有账号。用 KeyByAccount 的话
	//         所有游客会被归到同一个 subject（accountID=0）上共用一个桶 ——
	//         也就是"全世界游客一共 120 次/分钟"。那不是限流，是**误杀**。
	//         （这也是 ratelimit 包里 KeyByAccount 那条"必须在 JWTAuth 之后挂"
	//          警告的反面：这里根本不该用它。）
	//
	//  120/min：正常客户端**一次播放只打一次**（三条刷出路径共享同一个
	//         session_id、且有 flushed 标志，见前端 useQoE.ts）。
	//         所以 120/分钟意味着"一分钟内在同一个 IP 上播了 120 条视频"——
	//         那已经远超正常使用了。留这么宽是因为**宁可不限也不能误杀**：
	//         误杀的后果不是 429，而是**这条播放的数据静默消失**，
	//         而埋点数据丢了是找不回来的。
	//
	//         顺带：nat 后面（宿舍/公司/咖啡厅）很多用户共用一个出口 IP，
	//         这是把上限设宽的第二条理由。
	qoeReportLimiter := ratelimit.Limit(cache, "qoe_report", 120, time.Minute, ratelimit.KeyByIP)

	// ---------------- account ----------------
	// 依赖注入三连：handler 只认识 service，service 只认识 repo，repo 只认识 db
	accountRepository := account.NewAccountRepository(db)
	// cache 可能为 nil（启动降级）。nil 时 Login 不写缓存、Logout 不删缓存、
	// Refresh 走全表扫慢路径 —— 功能完整，只是慢。详见 account/service.go
	accountService := account.NewAccountService(accountRepository, cache)
	accountHandler := account.NewAccountHandler(accountService, cfg.Storage)

	accountGroup := r.Group("/account")
	{
		// 公开接口（无需登录）
		//
		// register/login 是**唯一两个按 IP 限流**的接口（见上面的策略表）：
		// 这里还没有可信的账号身份。中间件写在 handler 参数位（不是 handler 里），
		// 好处是被限流的请求**根本不会进入业务逻辑** —— 不会建账号、不会查库、
		// 不会算密码哈希，429 是这条路最便宜的一次返回。
		//
		// refresh/changePassword/findByID/findByUsername 不挂：
		// 前两个要凭 token 才能调，后两个是只读查询。
		accountGroup.POST("/register", registerLimiter, accountHandler.CreateAccount)
		accountGroup.POST("/login", loginLimiter, accountHandler.Login)
		accountGroup.POST("/refresh", accountHandler.Refresh)
		accountGroup.POST("/changePassword", accountHandler.ChangePassword)
		accountGroup.POST("/findByID", accountHandler.FindByID)
		accountGroup.POST("/findByUsername", accountHandler.FindByUsername)
	}
	protectedAccountGroup := accountGroup.Group("")
	protectedAccountGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// 受保护接口（中间件验完身份，handler 直接用）
		protectedAccountGroup.POST("/logout", accountHandler.Logout)
		protectedAccountGroup.POST("/rename", accountHandler.Rename)
		protectedAccountGroup.POST("/uploadAvatar", accountHandler.UploadAvatar)
		protectedAccountGroup.POST("/updateProfile", accountHandler.UpdateProfile)
	}
	// getProfile 跨模块聚合接口在下面 social 段之后注册（依赖 socialRepository）

	// ---------------- video ----------------
	videoRepository := video.NewVideoRepository(db)
	// 第二个参数是 VectorIndexer（发布后异步补向量），**本轮传 nil**。
	//
	// nil 的语义是"向量检索没启用"，不是"漏接了" —— indexAsync 开头就判 nil 直接返回，
	// 所以发布路径完全不受影响，也不会每条视频打一行错误日志。
	// 这和 Redis/MQ 在本项目里一贯的地位一致：可选组件缺席 = 少一个能力，不是故障。
	// 接线点在下面 search 那一段（搜到 "三个 nil" 就知道该改哪里）。
	// 阶段7：第三个参数 cache 开始传真 client —— GetDetail 的详情缓存 + 防击穿锁
	// 就是从这个参数接进去的。cache 可能为 nil（启动降级），各方法自己判空
	//
	// 扩展：第四个参数 storage 是上传根目录。Publish 要把它和 PlayURL
	// 拼成磁盘路径才能去探测源文件（上传质量门禁）。传值不是指针 ——
	// 它只有一个字符串字段，值语义意味着发布过程中它不可能被别处改掉。
	// 注意**这个参数不是可选的**：cfg.Storage 在 config.Load 里已经
	// 填好默认值，所以这里拿到的永远是有效路径。
	videoService := video.NewVideoService(videoRepository, nil, cache, cfg.Storage)
	// 扩展：两个 handler 都多收一个 cfg.Storage —— 它们要算文件落地目录，
	// 必须和门禁探测读的根目录同源（见 config/paths.go）。
	videoHandler := video.NewVideoHandler(videoService, accountService, cfg.Storage) // accountService 死依赖，对齐原项目
	chunkHandler := video.NewChunkUploadHandler(cfg.Storage)                         // 阶段7回填：签名加 cache 参数

	videoGroup := r.Group("/video")
	{
		// 公开接口
		videoGroup.POST("/listByAuthorID", videoHandler.ListByAuthorID)
		videoGroup.POST("/getDetail", videoHandler.GetDetail)
	}
	protectedVideoGroup := videoGroup.Group("")
	protectedVideoGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// 受保护接口：直传 + 分片上传
		protectedVideoGroup.POST("/uploadVideo", videoHandler.UploadVideo)
		protectedVideoGroup.POST("/uploadCover", videoHandler.UploadCover)
		protectedVideoGroup.POST("/publish", videoHandler.PublishVideo)
		protectedVideoGroup.POST("/delete", videoHandler.DeleteVideo)
		// 挂在这里而不是一个新组：它就是单条删除的复数形态，
		// 鉴权要求和归属判断完全一样（作者本人）。**必须在 JWTAuth 组里** ——
		// 归属判断要靠 jwt.GetAccountID，软鉴权下游客拿到 0，
		// 那所有视频就都不是"他的"，批量删除会安静地变成"全部跳过"
		protectedVideoGroup.POST("/deleteBatch", videoHandler.DeleteVideosBatch)
		protectedVideoGroup.POST("/chunk/init", chunkHandler.InitChunkUpload)
		protectedVideoGroup.POST("/chunk/upload", chunkHandler.UploadChunk)
		protectedVideoGroup.POST("/chunk/status", chunkHandler.ChunkStatus)
		protectedVideoGroup.POST("/chunk/complete", chunkHandler.CompleteChunkUpload)
	}

	// ---------------- like ----------------
	// 点赞复用 video 包的 repository（likes 表和 likes_count 都属于视频模块的数据）。
	// 注意 likeService 的第二个依赖是 videoRepository —— 点赞事务里要"手工模拟外键"
	// 检查视频是否还存在，所以这两个 repo 必须进同一个 service。
	likeRepository := video.NewLikeRepository(db)

	// 阶段9：两条 MQ 链路。**各自独立构造、各自独立降级** ——
	// 一条声明失败不影响另一条，也不影响服务启动。
	//
	// 为什么不像其他组件那样在 main.go 里一次性建好：因为每个 NewXxxMQ
	// 都要在 broker 上声明自己的拓扑，是一个可能失败的网络动作。
	// 放在组装点（这里）而不是 main.go，失败的影响范围就精确限制在
	// 用到它的那个 service 上 —— 这一处 MQ 声明失败，点赞降级，
	// 但评论、关注照样能走它们的 MQ。
	likeMQ, err := rabbitmq.NewLikeMQ(rmq)
	if err != nil {
		log.Printf("LikeMQ 初始化失败（点赞落库降级为同步直写）: %v", err)
		likeMQ = nil
	}
	popularityMQ, err := rabbitmq.NewPopularityMQ(rmq)
	if err != nil {
		log.Printf("PopularityMQ 初始化失败（热度降级为同步直写）: %v", err)
		popularityMQ = nil
	}

	likeService := video.NewLikeService(likeRepository, videoRepository, cache, likeMQ, popularityMQ)
	likeHandler := video.NewLikeHandler(likeService)

	likeGroup := r.Group("/like")
	// 这个空路径子组不是多余的：它是**只给受保护路由挂中间件的锚点**。
	// likeGroup 本身不挂 JWTAuth，将来若要加公开的点赞数查询，直接挂 likeGroup 就行。
	protectedLikeGroup := likeGroup.Group("")
	protectedLikeGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// 两个写接口：30次/分/账号。like 和 unlike **共用同一个限流器**，
		// 所以"点30次赞再点30次取消赞"是 60 次写入 —— 会撞限流。
		// 这是有意的：对 Redis/MySQL 的压力不分正负，反复横跳就是攻击形态。
		//
		// 中间件在 JWTAuth 之后（group.Use 先于路由参数），所以 KeyByAccount
		// 能拿到 accountID。这两个的顺序反了不会报错，只会静默不限流。
		protectedLikeGroup.POST("/like", likeLimiter, likeHandler.Like)
		protectedLikeGroup.POST("/unlike", likeLimiter, likeHandler.Unlike)
		// 两个读接口：不限流
		protectedLikeGroup.POST("/isLiked", likeHandler.IsLiked)
		protectedLikeGroup.POST("/listMyLikedVideos", likeHandler.ListMyLikedVideos)
	}

	// ---------------- comment ----------------
	// commentHandler 的第二个依赖是 accountService —— 全项目 handler 跨模块依赖
	// service 的唯一样板（写时冗余：accountID → username）。见 comment_handler.go。
	//
	// notificationRepository 也在这里构造，但**只交给 commentService**，
	// 不进 handler：通知是评论的副作用，前端没有任何接口能直接写通知。
	notificationRepository := notification.NewNotificationRepository(db)
	commentRepository := video.NewCommentRepository(db)

	// 阶段9：评论链路的两条 MQ，和点赞那边同构、同规矩 ——
	// 各自独立构造、各自独立降级，一条声明失败不影响另一条。
	//
	// 注意 popularityMQ 是**第二个 PopularityMQ 实例**（点赞那边已经有一个）。
	// 不是笔误：每个实例持有自己的一条 channel，而 channel 在 amqp 里
	// **不是并发安全的**（多个 goroutine 同时 publish 会互相踩坏帧边界）。
	// 复用同一个实例看似省事，实际是在两个 HTTP handler 之间共享一条 channel，
	// 高并发下会随机报 "unexpected command" 之类的错。多开一条 channel
	// 的成本可以忽略，所以宁可重复构造。
	commentMQ, err := rabbitmq.NewCommentMQ(rmq)
	if err != nil {
		log.Printf("CommentMQ 初始化失败（评论落库降级为同步直写）: %v", err)
		commentMQ = nil
	}
	commentPopularityMQ, err := rabbitmq.NewPopularityMQ(rmq)
	if err != nil {
		log.Printf("CommentPopularityMQ 初始化失败（评论热度降级为同步直写）: %v", err)
		commentPopularityMQ = nil
	}

	commentService := video.NewCommentService(commentRepository, videoRepository, accountRepository, notificationRepository, cache, commentMQ, commentPopularityMQ)
	commentHandler := video.NewCommentHandler(commentService, accountService)

	commentGroup := r.Group("/comment")
	{
		// 公开：看评论不需要登录（和 /video/getDetail 同类）。
		// 顺带注意它**不挂限流** —— 阶段7 只给两个写接口挂 commentLimiter。
		commentGroup.POST("/listAll", commentHandler.GetAllComments)
	}
	protectedCommentGroup := commentGroup.Group("")
	protectedCommentGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// 10次/分/账号，**比点赞严 3 倍**。依据是副作用成本不同：
		// 一次点赞 = 1 行 like + 自增，一次评论 = 1 行 comment + 通知落库 + 热度。
		// 限流阈值不该按"业务上多久操作一次"拍，要按"一次操作在系统里炸开多大"。
		protectedCommentGroup.POST("/publish", commentLimiter, commentHandler.PublishComment)
		protectedCommentGroup.POST("/delete", commentLimiter, commentHandler.DeleteComment)
	}

	// ---------------- social ----------------
	// 关注模块只依赖 accountRepository —— 它没有任何别的模块的数据。
	// 但**别人依赖它**：feed 的关注流（子查询读 socials 表）、getProfile 的两个计数。
	// 一个薄模块被三处引用，是"数据所有权"和"调用次数"不成正比的典型。
	socialRepository := social.NewSocialRepository(db)

	// 阶段9：关注链路的 MQ。**它和上面两条的性质不一样** ——
	// like/comment 的 MQ 是"唯一写路径"（范式 A），这条是"冗余双写"（范式 B），
	// 失败只记日志。详见 internal/social/service.go 类型注释。
	socialMQ, err := rabbitmq.NewSocialMQ(rmq)
	if err != nil {
		log.Printf("SocialMQ 初始化失败（关注照常同步落库，仅通知链路缺失）: %v", err)
		socialMQ = nil
	}

	socialService := social.NewSocialService(socialRepository, accountRepository, cache, socialMQ)
	socialHandler := social.NewSocialHandler(socialService)

	socialGroup := r.Group("/social")
	// 整个组都是强鉴权 —— 和 /like 组同类。关注关系一定有"我"，
	// 没有匿名看关注列表这种场景（getAllFollowers 传 0 的意思是"我自己"，
	// 那也需要先知道"我"是谁）。
	protectedSocialGroup := socialGroup.Group("")
	protectedSocialGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// 20次/分/账号。注意关注**没有**点赞/取消赞那种"横跳"困扰：
		// 关注是有向关系，follow→unfollow→follow 会反复写 socials 表并触发
		// 关注流缓存失效（DelByPattern），这正是一个人能被限流按住的最贵操作。
		protectedSocialGroup.POST("/follow", socialLimiter, socialHandler.Follow)
		protectedSocialGroup.POST("/unfollow", socialLimiter, socialHandler.Unfollow)
		// 三个读接口：不限流
		protectedSocialGroup.POST("/getAllFollowers", socialHandler.GetAllFollowers)
		protectedSocialGroup.POST("/getAllVloggers", socialHandler.GetAllVloggers)
		protectedSocialGroup.POST("/getCounts", socialHandler.GetCounts)
	}

	// ---------------- message（阶段12） ----------------
	// 全项目**最后一个业务模块**，也是依赖最少的一个：只要 db 和 accountRepository。
	// 没有缓存、没有 MQ、没有跨模块数据 —— 它是"四件套最朴素形态"的实物样本。
	//
	// 依赖链可以在这里一眼看完：
	//	messageRepository ← db
	//	messageService    ← messageRepository + accountRepository（校验收件人存在）
	//	messageHandler    ← messageService
	// 没有任何一步是可选的。**对比 video 那一坨**（db + cache + 三个 MQ + worker），
	// 就能看出"模块的接线复杂度 = 它对外部状态的依赖数"。
	messageRepository := message.NewMessageRepository(db)
	messageService := message.NewMessageService(messageRepository, accountRepository)
	messageHandler := message.NewMessageHandler(messageService)

	messageGroup := r.Group("/message")
	// ⚠ 和上面所有组都不同的地方：**整个组套 JWTAuth，没有一条公开路由。**
	//
	// like/comment/social 都是"公开读 + 鉴权写"的两层结构，因为它们读的是
	// 公开内容。私信是私有数据，**匿名读一个字节都是漏洞**，所以这里没有
	// protected/public 的分层，组本身就只有一层。
	//
	// 这条差异不是风格选择，是数据属性的直接映射 —— 见 message/handler.go 顶上那段。
	protectedMessageGroup := messageGroup.Group("")
	protectedMessageGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		// **两条都不限流。** 原项目如此，本项目照抄 —— 但要知道这是个
		// 有意的留白而不是疏漏：私信是唯一"写得越快越正常"的接口
		// （聊天本来就是连续敲字），给它套一个 10/min 的桶等于把聊天功能废掉。
		//
		// 真要防滥用，正确的方向不是"按次数限流"，而是：
		//   ① 按内容查重（同样的文本连发 N 次 → 拒）
		//   ② 按未读数限（对方 100 条没读 → 拒，防止单方面轰炸）
		// 两者都需要读历史数据，是**业务规则**不是中间件规则 ——
		// 所以它不该长成 ratelimit 包里的一个桶。
		// **限流器只能表达"频率"，表达不了"关系"。**
		protectedMessageGroup.POST("/send", messageHandler.Send)
		protectedMessageGroup.POST("/list", messageHandler.List)
	}

	// ---------------- getProfile（跨模块聚合） ----------------
	// 挂在 accountGroup 上（公开，无鉴权）—— 看别人的主页不需要登录。
	//
	// **注意它在文件里的位置**：accountGroup 在第 39 行就建好了，但这条路由要到
	// 这里才能注册 —— 因为它的 handler 依赖 socialRepository（上面刚建）。
	// gin 允许往一个已存在的 Group 里继续加路由，只要在 r.Run() 之前。
	//
	// 聚合逻辑在 internal/profile 包里，不在这里内联：原项目把这段写在 router 里，
	// 理由是"它不属于任何单一模块"，但代价是 router.go 里混进了业务逻辑和错误策略。
	// 详见 internal/profile/handler.go 顶上的说明（里面还解释了为什么
	// 文档建议的"挪进 accountHandler"在本项目**编译不过**）。
	profileHandler := profile.NewProfileHandler(accountService, videoRepository, socialRepository)
	accountGroup.POST("/getProfile", profileHandler.GetProfile)

	// ---------------- search ----------------
	// search 是 provider 包：没有表、没有自己的路由组、没有 handler。
	// 它的 HTTP 入口挂在下面的 feed 组里（/feed/search），
	// 理由见 internal/search/entity.go 的包注释（搜索结果必须是 FeedVideoItem）。
	//
	// ---------- 本轮只接了词法那一路 ----------
	//
	// 后三个参数（vec / embed / marker）全传 nil，语义是"语义那一路没启用"：
	// service.go 里 runVec 恒为 false，模式只会落在 ngram / ngram-or / like 三个上。
	// 向量那一路（Redis Stack + Ollama + 回填命令）本轮先跳过。
	//
	// **注意这三个 nil 不是占位符摆设，它们就是接线点**：
	// 把 nil 换成 search.NewVectorStore(...) / search.NewOllamaEmbedder(...) /
	// videoRepository，其余一行不用改 —— service.go 的降级逻辑早就按
	// "vec 或 embed 为 nil 就整路跳过"写好了。这也是当初选择传 nil 而不是
	// 删掉向量代码的理由：代价是三行 nil，收益是接线时不需要重构。
	searchLexical := search.NewLexicalSearcher(db)
	searchService := search.NewService(searchLexical, nil, nil, nil, search.Config{
		Depth: cfg.Search.Depth,
		RRFK:  cfg.Search.RRFK,
	})
	// 启动日志把**降级形态**报出来：这是排查"搜索怎么变差了"的第一现场。
	// 一个可选组件缺席时的表现如果是"安静地少一半能力"，那它迟早会被当成语料问题。
	lexicalEnabled, vectorEnabled := searchService.Enabled()
	log.Printf("[search] 词法路启用=%v 向量路启用=%v", lexicalEnabled, vectorEnabled)

	// ---------------- qoe（扩展，无阶段编号）----------------
	//
	// 播放质量埋点。四件套齐全但**这一段的形状和别的模块不一样**，
	// 值得看清楚：它整个组只挂 **SoftJWTAuth**，没有再分 public/protected ——
	// 因为两个接口都该对游客开放（report 要收游客的数据，stats 是公开聚合）。
	//
	// 对比一下全项目各个组的鉴权形状，这是第四种：
	//
	//	account/video/like/comment/social  public 组 + protected 子组（两段式）
	//	message                            整体 JWTAuth（全私有，连公开层都没有）
	//	feed                               整体 SoftJWTAuth + protected 子组（两段式，软）
	//	**qoe**                            整体 SoftJWTAuth，**没有子组**（全软）
	//
	// 第四种是"整个模块都是软的"，本项目里独此一家。它成立的前提是
	// **这个模块没有任何一件事需要"确定知道你是谁"** ——
	// report 只需要"可能是你"（能认出来更好，认不出按游客算），
	// stats 根本不需要身份。一旦将来加一个"查我的播放历史"的接口，
	// 就必须长出 protected 子组了。
	qoeRepository := qoe.NewQoERepository(db)
	qoeService := qoe.NewQoEService(qoeRepository)
	qoeHandler := qoe.NewQoEHandler(qoeService)

	qoeGroup := r.Group("/qoe")
	qoeGroup.Use(jwt.SoftJWTAuth(accountRepository, cache))
	{
		// report 挂限流：它是**唯一一个"客户端说多少就是多少"的写接口**，
		// 也是唯一一个不鉴权就能往库里写数据的接口。别的公开写接口
		// （register/login）都有身份或强限流兜着。
		//
		// stats 不挂 —— 它是读接口，按项目既定纪律"读接口一律不挂"。
		// 它也是全表扫描的重查询，理论上可以被拿来打，但那需要的请求量
		// 远超"正常用户误伤"的阈值，真担心应该在库侧加缓存而不是在入口限流
		// （对比：feed 的重查询靠 Redis 缓存扛，不靠限流）。
		qoeGroup.POST("/report", qoeReportLimiter, qoeHandler.Report)
		qoeGroup.POST("/stats", qoeHandler.Stats)
	}

	// ---------------- feed ----------------
	// feed 不拥有任何表：它只读 videos/tags，所以这里没有 AutoMigrate 相关的东西
	feedRepository := feed.NewFeedRepository(db)
	feedService := feed.NewFeedService(feedRepository, likeRepository, searchService, cache) // 阶段7回填：加 cache
	feedHandler := feed.NewFeedHandler(feedService)

	feedGroup := r.Group("/feed")
	// 注意是 SoftJWTAuth：游客也要能刷流。
	// 它和 JWTAuth 的区别只在"没 token 时"——JWTAuth 直接 401 掐断，
	// SoftJWTAuth 放行并把 viewerAccountID 留空（handler 里降级成 0）
	feedGroup.Use(jwt.SoftJWTAuth(accountRepository, cache))
	{
		feedGroup.POST("/listLatest", feedHandler.ListLatest)
		feedGroup.POST("/listLikesCount", feedHandler.ListLikesCount)
		feedGroup.POST("/listByPopularity", feedHandler.ListByPopularity)
		feedGroup.POST("/listByTag", feedHandler.ListByTag)
		// 混合检索（本轮只有词法那一路）。**挂在这里而不是 protectedFeedGroup**：
		// 搜索的是公开数据，游客也应该能搜 —— 和 listByTag 同类。
		// 放进受保护组会让登出状态下搜索直接 401，而这不是任何安全边界上的东西。
		feedGroup.POST("/search", feedHandler.Search)
	}
	// 关注流（阶段6）：**同一组里唯一必须登录的接口**。
	//
	// 这个空路径子组是**双重鉴权**的锚点：feedGroup 整体挂了 SoftJWTAuth
	// （游客可读，拿不到身份就降级成 0），这里再叠一层 JWTAuth ——
	// 因为"我关注的人"必须先有"我"，匿名问这个问题没有意义。
	//
	// gin 的中间件按注册顺序执行：先 SoftJWTAuth（放行、可能塞 accountID），
	// 再 JWTAuth（没 token 就掐断）。两层在同一请求上跑，谁都拦得住。
	// 这也是"认证强度可以逐组叠加"的示范 —— 和 /like、/social、/comment 的
	// 写接口不同：那几组是整体强鉴权，没有软硬混用的组。
	protectedFeedGroup := feedGroup.Group("")
	protectedFeedGroup.Use(jwt.JWTAuth(accountRepository, cache))
	{
		protectedFeedGroup.POST("/listByFollowing", feedHandler.ListByFollowing)
	}

	// ---------------- timeline + outbox（阶段9）----------------
	//
	// 这一段跑在 **API 进程的后台 goroutine** 里，不是 worker 进程 ——
	// 判断标准见 internal/worker/outboxworker.go 文件头：
	// **要共享 API 进程的资源（Redis 连接 / SSE 长连接 / 内存注册表）
	// 就必须留在 API 进程**，因为那些资源跨不了进程。
	//
	// 注意这两个调用**不阻塞**（内部各起一个 goroutine），
	// 所以它们可以在这里、在 r.Run() 之前安心调用。
	timelineMQ, err := rabbitmq.NewTimelineMQ(rmq)
	if err != nil {
		log.Printf("TimelineMQ 初始化失败（发布走不了 MQ，outbox 行会持续积压）: %v", err)
		timelineMQ = nil
	}

	// 转码链路的发布端（扩展）。
	//
	// **它只需要在 API 进程里建**：这条队列的**消费者在 worker 进程**
	// （转码是分钟级 CPU 任务，见 internal/worker/transcode_worker.go），
	// 而发布者是本进程的 outbox 轮询器。worker 那边不需要这个封装 ——
	// 它不发布，只消费，拓扑由 DeclareAllTopology 声明。
	//
	// 失败降级同 timelineMQ：置 nil，轮询器会给 video_transcode 类事件
	// 返回"目标不可用"，那类行会积压而不丢（**注意不是整个轮询器停摆**，
	// 其余事件照常投递 —— 见 StartOutboxPoller 的说明）。
	transcodeMQ, err := rabbitmq.NewTranscodeMQ(rmq)
	if err != nil {
		log.Printf("TranscodeMQ 初始化失败（高码率视频会一直停在 pending）: %v", err)
		transcodeMQ = nil
	}

	// 轮询器：把 outbox_msgs 里 pending 的行按 EventType 搬到对应的 MQ。
	// 全部目标都 nil 时它内部直接 return —— **注意这不是"静默吞掉"**：
	// 行会留在表里一条不丢，等 MQ 恢复后重启 API 就补投。见函数注释。
	worker.StartOutboxPoller(db, worker.OutboxPublishers{
		Timeline:  timelineMQ,
		Transcode: transcodeMQ,
	})

	// 消费者：把 timeline 事件变成 Redis ZSet 里的一条。
	// 它写的是 feed:global_timeline，读它的是 feed.ListLatest。
	worker.StartConsumer(timelineMQ, timelineQueueName, cache, rmq)

	// ---------------- notification + SSE（阶段9）----------------
	//
	// 三条**通知副本队列**：绑在业务交换机上（like.events / comment.events /
	// social.events），但**只绑正向动作** —— 取消赞/删评论/取关不产生通知。
	//
	// 为什么必须是"另开的队列"而不是复用业务队列，见 topology.go 里
	// notificationLinks 的说明（一句话：同队列是竞争消费，会把消息抢走）。
	//
	// 声明失败**只记日志不中断启动**：通知队列建不起来，点赞/评论/关注
	// 照样能跑（它们只依赖业务队列）。这是"一条 MQ 链路失败不牵连别人"
	// 这条纪律在这里的又一次兑现。
	if rmq != nil {
		notifCh, err := rmq.NewChannel()
		if err != nil {
			log.Printf("通知队列声明失败（通知子系统不可用）: %v", err)
		} else {
			if err := rabbitmq.DeclareNotificationQueues(notifCh); err != nil {
				log.Printf("通知队列声明失败（通知子系统不可用）: %v", err)
			}
			_ = notifCh.Close() // 拓扑长在 broker 上，不依赖这条临时 channel
		}
	}

	sseHub := worker.NewSSEHub(notificationRepository, accountRepository, cache)

	// 3 个消费者，跑在 API 进程的 goroutine 里，各自独立 channel + 5 秒重连。
	// rmq == nil 时内部整体跳过（通知**没有**降级直写路径，理由见函数注释）。
	worker.StartNotificationConsumers(rmq, videoRepository, notificationRepository, sseHub, []string{
		notificationLikeQueueName,
		notificationCommentQueueName,
		notificationSocialQueueName,
	})

	// 通知中心的四个接口。**鉴权用的是 QueryTokenAuth 而不是 JWTAuth** ——
	// EventSource 发不了自定义 header，token 只能从 ?token= 传。
	// 两者的撤销检查走的是同一份代码（见 jwt.QueryTokenAuth 的说明）。
	notifGroup := r.Group("/notification")
	notifGroup.Use(sseHub.SSERequireAuth())
	sseHub.RegisterRoutes(notifGroup)

	return r
}
