package http

import (
	"log"

	"myfeed/internal/account"
	"myfeed/internal/config"
	"myfeed/internal/feed"
	jwt "myfeed/internal/middleware/jwt"
	"myfeed/internal/notification"
	"myfeed/internal/profile"
	"myfeed/internal/search"
	"myfeed/internal/social"
	"myfeed/internal/video"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
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
// 阶段7回填：签名加 cache *rediscache.Client；阶段9回填：签名加 rmq *rabbitmq.RabbitMQ
func SetRouter(db *gorm.DB, cfg config.Config) *gin.Engine {
	r := gin.Default()
	if err := r.SetTrustedProxies(nil); err != nil {
		log.Printf("SetTrustedProxies failed: %v", err)
	}

	// 健康检查（部署时探活用，阶段10 的 healthcheck 会调它）
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// 静态文件服务：上传的头像/视频/封面都落在 .run/uploads 下
	// URL 形如 /static/avatars/1/abcd1234.png
	r.Static("/static", "./.run/uploads")

	// ---------------- account ----------------
	// 依赖注入三连：handler 只认识 service，service 只认识 repo，repo 只认识 db
	accountRepository := account.NewAccountRepository(db)
	accountService := account.NewAccountService(accountRepository)
	accountHandler := account.NewAccountHandler(accountService)

	accountGroup := r.Group("/account")
	{
		// 公开接口（无需登录）
		// 阶段7回填：register 挂 5次/时/IP、login 挂 10次/分/IP 的限流中间件
		accountGroup.POST("/register", accountHandler.CreateAccount)
		accountGroup.POST("/login", accountHandler.Login)
		accountGroup.POST("/refresh", accountHandler.Refresh)
		accountGroup.POST("/changePassword", accountHandler.ChangePassword)
		accountGroup.POST("/findByID", accountHandler.FindByID)
		accountGroup.POST("/findByUsername", accountHandler.FindByUsername)
	}
	protectedAccountGroup := accountGroup.Group("")
	protectedAccountGroup.Use(jwt.JWTAuth(accountRepository))
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
	videoService := video.NewVideoService(videoRepository, nil)
	videoHandler := video.NewVideoHandler(videoService, accountService) // accountService 死依赖，对齐原项目
	chunkHandler := video.NewChunkUploadHandler()                       // 阶段7回填：签名加 cache 参数

	videoGroup := r.Group("/video")
	{
		// 公开接口
		videoGroup.POST("/listByAuthorID", videoHandler.ListByAuthorID)
		videoGroup.POST("/getDetail", videoHandler.GetDetail)
	}
	protectedVideoGroup := videoGroup.Group("")
	protectedVideoGroup.Use(jwt.JWTAuth(accountRepository))
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
	likeService := video.NewLikeService(likeRepository, videoRepository) // 阶段7回填：加 cache + likeLimiter；阶段9回填：加 likeMQ/popularityMQ
	likeHandler := video.NewLikeHandler(likeService)

	likeGroup := r.Group("/like")
	// 这个空路径子组不是多余的：它是**只给受保护路由挂中间件的锚点**。
	// likeGroup 本身不挂 JWTAuth，将来若要加公开的点赞数查询，直接挂 likeGroup 就行。
	protectedLikeGroup := likeGroup.Group("")
	protectedLikeGroup.Use(jwt.JWTAuth(accountRepository))
	{
		// 两个写接口：阶段7 回填 30次/分/账号 的限流
		protectedLikeGroup.POST("/like", likeHandler.Like)
		protectedLikeGroup.POST("/unlike", likeHandler.Unlike)
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
	commentService := video.NewCommentService(commentRepository, videoRepository, accountRepository, notificationRepository) // 阶段7回填：加 cache；阶段9回填：加 commentMQ/popularityMQ
	commentHandler := video.NewCommentHandler(commentService, accountService)

	commentGroup := r.Group("/comment")
	{
		// 公开：看评论不需要登录（和 /video/getDetail 同类）。
		// 顺带注意它**不挂限流** —— 阶段7 只给两个写接口挂 commentLimiter。
		commentGroup.POST("/listAll", commentHandler.GetAllComments)
	}
	protectedCommentGroup := commentGroup.Group("")
	protectedCommentGroup.Use(jwt.JWTAuth(accountRepository))
	{
		// 阶段7回填：这两个路由各挂 commentLimiter（"comment_write", 10次/分, KeyByAccount）
		protectedCommentGroup.POST("/publish", commentHandler.PublishComment)
		protectedCommentGroup.POST("/delete", commentHandler.DeleteComment)
	}

	// ---------------- social ----------------
	// 关注模块只依赖 accountRepository —— 它没有任何别的模块的数据。
	// 但**别人依赖它**：feed 的关注流（子查询读 socials 表）、getProfile 的两个计数。
	// 一个薄模块被三处引用，是"数据所有权"和"调用次数"不成正比的典型。
	socialRepository := social.NewSocialRepository(db)
	socialService := social.NewSocialService(socialRepository, accountRepository) // 阶段7回填：加 cache；阶段9回填：加 socialMQ
	socialHandler := social.NewSocialHandler(socialService)

	socialGroup := r.Group("/social")
	// 整个组都是强鉴权 —— 和 /like 组同类。关注关系一定有"我"，
	// 没有匿名看关注列表这种场景（getAllFollowers 传 0 的意思是"我自己"，
	// 那也需要先知道"我"是谁）。
	protectedSocialGroup := socialGroup.Group("")
	protectedSocialGroup.Use(jwt.JWTAuth(accountRepository))
	{
		// 两个写接口：阶段7 回填 20次/分/账号 的 socialLimiter
		protectedSocialGroup.POST("/follow", socialHandler.Follow)
		protectedSocialGroup.POST("/unfollow", socialHandler.Unfollow)
		// 三个读接口：不限流
		protectedSocialGroup.POST("/getAllFollowers", socialHandler.GetAllFollowers)
		protectedSocialGroup.POST("/getAllVloggers", socialHandler.GetAllVloggers)
		protectedSocialGroup.POST("/getCounts", socialHandler.GetCounts)
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

	// ---------------- feed ----------------
	// feed 不拥有任何表：它只读 videos/tags，所以这里没有 AutoMigrate 相关的东西
	feedRepository := feed.NewFeedRepository(db)
	feedService := feed.NewFeedService(feedRepository, likeRepository, searchService) // 阶段7回填：加 cache
	feedHandler := feed.NewFeedHandler(feedService)

	feedGroup := r.Group("/feed")
	// 注意是 SoftJWTAuth：游客也要能刷流。
	// 它和 JWTAuth 的区别只在"没 token 时"——JWTAuth 直接 401 掐断，
	// SoftJWTAuth 放行并把 viewerAccountID 留空（handler 里降级成 0）
	feedGroup.Use(jwt.SoftJWTAuth(accountRepository))
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
	protectedFeedGroup.Use(jwt.JWTAuth(accountRepository))
	{
		protectedFeedGroup.POST("/listByFollowing", feedHandler.ListByFollowing)
	}

	return r
}
