package http

import (
	"log"

	"myfeed/internal/account"
	jwt "myfeed/internal/middleware/jwt"
	"myfeed/internal/video"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SetRouter 依赖注入大汇合：new Repo → new Service → new Handler → 挂路由。
// 每完成一个模块，就在这里加一段组装 + 一组路由。
// 阶段7回填：签名加 cache *rediscache.Client；阶段9回填：签名加 rmq *rabbitmq.RabbitMQ
func SetRouter(db *gorm.DB) *gin.Engine {
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
	// getProfile 跨模块聚合接口：阶段6回填（依赖 video/social 的 repo 方法）

	// ---------------- video ----------------
	videoRepository := video.NewVideoRepository(db)
	videoService := video.NewVideoService(videoRepository)
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
		protectedVideoGroup.POST("/chunk/init", chunkHandler.InitChunkUpload)
		protectedVideoGroup.POST("/chunk/upload", chunkHandler.UploadChunk)
		protectedVideoGroup.POST("/chunk/status", chunkHandler.ChunkStatus)
		protectedVideoGroup.POST("/chunk/complete", chunkHandler.CompleteChunkUpload)
	}

	return r
}
