package db

import (
	"fmt"
	"time"

	"myfeed/internal/account"
	"myfeed/internal/config"
	"myfeed/internal/message"
	"myfeed/internal/notification"
	"myfeed/internal/qoe"
	"myfeed/internal/social"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"myfeed/internal/video"
)

// 连接池参数。
//
// ---------- 为什么这三个数是必须显式设的 ----------
//
// `gorm.Open` 之后**不设任何参数**的话，走的是 database/sql 的默认值：
//
//	MaxOpenConns    = 0   （无上限）
//	MaxIdleConns    = 2   （！！）
//	ConnMaxLifetime = 0   （永不过期）
//
// `MaxIdleConns=2` 是这里的元凶。压测 c=80 的时候，80 个并发要 80 条连接，
// 池子里只留 2 条空闲的 —— 剩下 78 条用完就被**真正关掉**。而关掉的 TCP 连接
// 不会立刻消失，它要在 TIME_WAIT 里躺 ~120 秒。Windows 的动态端口段只有 ~16384 个，
// 一次 16 接口的压测扫描就能把 TIME_WAIT 从 1687 推到 15502，然后：
//   - 应用自己连 MySQL 拨不出端口 → 大量 [500]
//   - hey 这个客户端也拨不出端口 → 状态码 000
//   - 于是同一条接口，前半程 5000 QPS、后半程 800，而代码一个字没改
//
// 所以 MaxOpenConns 和 MaxIdleConns **必须相等**：相等意味着"用过的连接全部留着"，
// 一条都不关，一个端口都不漏。只调大 MaxOpenConns 而让 MaxIdleConns 保持 2，
// 是治不好的 —— 那只是把"同时开 80 条"变成"同时开 80 条然后关掉 78 条"，
// 端口照样漏。
//
// ConnMaxLifetime 设 1 小时：再长的连接可能已经被 MySQL 的 wait_timeout 掐了
// （默认 8 小时，但中间还隔着 Docker 的 NAT 表老化），用一条死连接会拿到
// "invalid connection"。1 小时远小于任何一个超时，又能让连接复用足够久。
// 连接数取 100 而不是更小，有个非直觉的理由：**池子必须 ≥ 压测并发数**。
// 如果设成 50 而 hey 用 c=80 打，那么同时只有 50 个请求能进 DB、另外 30 个在
// 池子里排队 —— 这时你测到的瓶颈是"我设的连接池上限"，不是代码本身的性能，
// 两个 A/B 组的差异会被这个上限抹平或放大。设 100 是为了让 c=80 永远不排队。
// 反过来说，将来真要压 c=500，池子就该跟着调 —— 池子大小是一个**跟着负载走**的参数。
const (
	maxOpenConns    = 100
	maxIdleConns    = 100
	connMaxLifetime = time.Hour
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

	// gorm.Open 返回的是 GORM 句柄，连接池藏在它持有的 *sql.DB 里，
	// 必须取出来才能配。这一步失败说明驱动内部状态不对，属于启动期致命错误
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetConnMaxLifetime(connMaxLifetime)

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

		// 阶段12：私信表 messages。
		//
		// 到这里为止，"每加一个业务模块就要来这里加一行"这件事已经重复了六次
		// （likes / comments / notifications / socials / messages）。
		// 值得记下这个 AutoMigrate 列表的性质：**它是全项目唯一的"模块注册表"**，
		// 忘了加不会有任何编译期或启动期提示 —— 表不存在要等到第一次
		// 真实请求打进来才炸 `Table 'myfeed.messages' doesn't exist`。
		//
		// 迁移完要验证的是**两个方向各有一个单列索引**：
		//   show index from messages;  → 看到 idx_message_from(from_id)
		//                                和 idx_message_to(to_id)
		// 少一个，会话查询的 OR 就会有一条腿退化成全表扫（原因见
		// message/entity.go 里那两列的注释）。
		&message.Message{},

		// 扩展（无阶段编号）：播放质量埋点 qoe_events。
		//
		// **这是全项目唯一一张会无限增长的表** —— 每播放一次就落一行，
		// 不删除、不归档、没有 TTL。别的表都由用户动作驱动
		// （发视频、点赞、关注），量级受内容量约束；这张表由**观看量**驱动，
		// 而观看量天然比内容量大一到两个数量级。
		//
		// 所以在学习项目里它是无害的，但它也是全项目**第一个需要"想过期策略"
		// 的地方**：真上线时要做的是按天分区 + 定期 drop 分区，
		// 而不是 DELETE（DELETE 十万行会和线上查询抢锁）。
		// 现在不做，但要知道这条路存在 —— 和 search_index.go 里
		// "知道升级路径存在，比现在就升级更值钱"是同一种记录。
		//
		// 迁移完要验证的两件事：
		//   show index from qoe_events;
		//     → idx_qoe_session 必须是 **UNIQUE**（不是普通索引）——
		//       它是 upsert 的键，"重复上报不重复计数"全靠它。
		//       少了 UNIQUE，同一 session 的三条刷出会落三行，
		//       首帧 P95 被同一批数据重复计入，卡顿率分母翻三倍。
		//     → idx_qoe_video_time 必须是**两列复合** (video_id, created_at)，
		//       不是两个单列索引。查询永远带时间范围，单列走不动。
		&qoe.QoEEvent{},
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
