package db

import (
	"fmt"

	"myfeed/internal/account"
	"myfeed/internal/config"
	"myfeed/internal/notification"
	"myfeed/internal/social"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"myfeed/internal/video"
)

// NewDB 拼 DSN 并建立连接，返回 GORM 句柄（内部是连接池）
func NewDB(cfg config.DatabaseConfig) (*gorm.DB, error) {
	dsn :=
		fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
			cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DBName)

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	return db, nil
}

// AutoMigrate 把结构体同步成真实的表
func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&account.Account{},
		&video.Video{}, &video.Tag{}, &video.VideoTag{}, &video.OutboxMsg{},
		// 阶段4：likes 表。这里必须带上 —— 忘了加它不会编译报错，
		// 只会在第一次点赞时炸 "Table 'myfeed.likes' doesn't exist"。
		// 迁移完用 show index from likes 确认只有一个**复合**唯一索引
		// idx_like_video_account (video_id, account_id)，不是两个单列索引。
		&video.Like{},

		// 阶段5：评论 + 通知。
		//
		// comments 没有唯一索引（对比 likes）—— 这不是漏了，是产品语义：
		// 一个人可以在同一条视频下发多条评论，所以没有 1062 可以撞。
		// 用 show index from comments 能看到两个**单列**索引
		// （video_id、author_id）+ 一个多余的 username 索引（原项目照抄的冗余，
		// 没有任何查询按 username 过滤，详见 comment_entity.go）。
		&video.Comment{},

		// notifications 表比原项目早了几步落地：原项目把它跟 SSE/Worker 一起放在
		// 阶段10，而阶段5 的 @提及就已经要往里写了。所以实体单独成包
		// （internal/notification），阶段10 的 Worker/SSE 到时候写进同一个包，
		// 不用搬家 —— 见 notification/entity.go 顶上的说明。
		&notification.Notification{},

		// 阶段6：关注关系表 socials（GORM 按 Social 复数推导表名）。
		//
		// **迁移完必须验证一件事**：`show index from socials` 要看到
		// idx_social_follower_vlogger 是**一个两列**索引 (follower_id, vlogger_id)，
		// 而不是两个单列索引。GORM 的写法是在两个字段上分别打同名的
		// `uniqueIndex:idx_social_follower_vlogger` —— 同名才会合成一个复合索引，
		// 名字打错一个字符就变成两个各自唯一的单列索引，
		// 那意味着"同一个人不能关注两个人"和"两个人不能关注同一个人"，
		// 而且插入第二条时才会炸。和阶段4 的 likes 是同一个坑。
		&social.Social{},
	); err != nil {
		return err
	}

	// videos 的**全文索引**只能在 AutoMigrate 之后单独建，原因和它带来的后果：
	//
	//  1. GORM 表达不了 `WITH PARSER ngram`，所以这是全项目第一处手写 DDL。
	//     **从这一行起，"结构体 tag 是 schema 唯一来源"这条性质不再成立** ——
	//     videos 的索引现在有两个来源（entity.go 的 tag + 下面这个函数里的 ALTER）。
	//
	//  2. 它必须排在 AutoMigrate **之后**：表得先存在，否则 ALTER 直接失败。
	//
	//  3. 它**不返回错误**（永远是 nil）。搜索是可降级功能，
	//     没有 ALTER 权限不该让服务起不来 —— 函数内部记日志，查询侧还有第二道降级
	//     （投出 1191 时退化成纯向量）。完整的说明在 video/search_index.go 顶上。
	return video.EnsureFulltextIndex(db)
}

// CloseDB 关闭底层连接池
func CloseDB(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
