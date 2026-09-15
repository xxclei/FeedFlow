package feed

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"time"

	rediscache "myfeed/internal/middleware/redis"
	"myfeed/internal/search"
	"myfeed/internal/video"

	"github.com/patrickmn/go-cache"
	"golang.org/x/sync/singleflight"
)

// 阶段7：关注流响应缓存的参数。
//
// 和 video 详情缓存（video_service.go）是**同一个模式**，只有三个数不一样，
// 每个不一样的地方都有理由：
//
//	锁 TTL  500ms（详情是 2s）—— 关注流的回源是一条**带子查询的翻页 SQL**，
//	          比详情多一个 JOIN，但结果集更小；500ms 是文档定的值。
//	响应 TTL 24h（详情是 5min）—— 关注流的失效是**显式**的（Follow/Unfollow
//	          会删掉这个用户的全部页缓存），不靠过期兜底。所以 TTL 只是
//	          "万一漏删了，最坏脏多久"的上限，设长一点换更高的命中率。
//	          代价：真漏删了，用户最长 24 小时看不到新关注的人的动态。
const (
	feedCacheOpTimeout = 50 * time.Millisecond

	// popularityReadOpTimeout 是热榜读路径的超时：**80ms**。
	//
	// 比写路径的 50ms 宽，也刻意比关注流的 50ms 宽，理由只有一个：
	// 这条路径上要做 ZUNIONSTORE（O(N*K) 聚合），它是本项目里唯一
	// 一个"Redis 侧要花可观 CPU"的操作，不是纯 RTT。给它 80ms
	// 是为了让正常情况下的聚合**不被自己误杀**。
	//
	// 但超时**不能省**，它的价值只在 Redis 半死不活时体现：
	// TCP 连得上、命令发得出去、就是不回包 —— 这时没有超时，
	// 整个热榜接口会一直挂着，直到客户端放弃。而热榜的整个设计前提就是
	// "Redis 随时可能不可用，降级是正常状态而非异常"。
	popularityReadOpTimeout = 80 * time.Millisecond
)

const (
	followingCacheTTL = 24 * time.Hour

	followingLockTTL = 500 * time.Millisecond

	followingLockWaitStep   = 20 * time.Millisecond
	followingLockWaitRounds = 5
)

// FeedService Feed 业务：游标编解码、limit 归一化、响应组装。
//
// 阶段7 的两套缓存都接完了：
//
//	响应级：ListByFollowing 整个响应 JSON 进 Redis（红锁模式 + 防击穿锁）
//	实体级：GetVideoByIDs 三级缓存（L1 go-cache → L2 Redis → L3 MySQL）
//	        + ListLatest 冷热分离（feed:global_timeline ZSET）
//
// 字段的多寡正好反映这两套的分工：响应级只需要 rediscache 一个字段，
// 实体级还要 localcache 和 requestGroup 两个**进程内**的组件。
// 那不是重复建设 —— 见 entity_cache.go 顶上"一致性半径的三级"那段。
type FeedService struct {
	repo *FeedRepository

	// likeRepo 是阶段4 接进来的（is_liked 批量查询）。
	// 它属于 video 包而不是 feed 包：likes 表是视频模块的数据，
	// feed 只是**消费**方 —— 所以这里依赖的是 video.LikeRepository，
	// feed 自己没有也不该有 likes 的 repo。
	likeRepo *video.LikeRepository

	// searchSvc 本轮接进来的混合检索。它为 nil 时 Search 返回错误，
	// 而不是静默返回空结果（"搜索坏了"和"没有结果"必须能分开）
	searchSvc *search.Service

	// rediscache 可以为 nil（启动降级）—— 所有用到它的地方都先判空走纯 DB
	rediscache *rediscache.Client
	// cacheTTL 做成字段而不是直接引常量：测试里要能调小它来验"过期后回源"
	cacheTTL time.Duration

	// localcache 是 GetVideoByIDs 的 L1（进程内）。它**只在这一个进程里有效**，
	// 所以多实例部署时各有一份，互相看不见 —— 这正是它 TTL 只有 5 秒的原因
	// （别的实例改了数据，你这份只能等它自己过期）。
	localcache *cache.Cache

	// requestGroup 是 singleflight 的合并组。**零值时即可用**（不需要 New），
	// 所以这里的零值就位是刻意的，不是漏初始化。
	// 它合并的是"同一进程内对同一个 key 的并发回源"：
	// sf:entity:<id>（批量实体）、sf:cold:listLatest:*（冷区翻页）、
	// sf:fallback:global_timeline_rebuild（时间线自举重建）。
	requestGroup singleflight.Group
}

func NewFeedService(repo *FeedRepository, likeRepo *video.LikeRepository, searchSvc *search.Service, rediscache *rediscache.Client) *FeedService {
	return &FeedService{
		repo:       repo,
		likeRepo:   likeRepo,
		searchSvc:  searchSvc,
		rediscache: rediscache,
		cacheTTL:   followingCacheTTL,
		// 即使 rediscache == nil 也把 L1 建出来（代价是每 5 秒一次空扫描）。
		// 不在这里做条件初始化，是为了让"要不要用 L1"这个判断**只存在于
		// 用到它的地方**（GetVideoByIDs 开头的 rediscache == nil 分支）。
		// 构造函数里埋条件会让"Redis 关了 L1 还在不在"变成要读两处才能回答的问题。
		localcache: cache.New(l1EntryTTL, l1SweepInterval),
	}
}

// ListLatest 最新流（时间游标）—— **冷热分离**。
//
// 主页是本站最热的路由，而它是唯一一条"数据量随时间无限增长"的流：
// 标签流、关注流都有天然的上界（标签就那么多、关注的人就那么多），
// 时间线没有 —— 三年后就是几百万条。
//
// 所以这条流不能整体进缓存（存不下），也不能整体查库（第 1 页和第 5 万页
// 的访问频率差几个数量级却付同样的代价）。冷热分离就是为这个形状设计的：
//
//	热区 = feed:global_timeline ZSET 里最近 timelineSize 条（内存装得下、常被访问）
//	冷区 = 更老的部分（装不下、几乎没人翻）
//
// ---------- 判断冷热的那一步：为什么不是先读水位线 ----------
//
// 文档和原项目都是"先读水位线（ZRANGE 0 0，拿最老那条的 score），
// 再拿游标和它比大小决定走哪条路"。逻辑上没错，但它**恒定花 2 次 Redis 往返**：
// 一次读水位、一次取 id。
//
// 本项目实测（`bench.bat latest` + `redis-cli INFO commandstats`）显示这条路由
// 的瓶颈已经不是 SQL 而是 **Redis 往返次数** —— 热路径总共 3 次：
//
//	1 次 JWT token 缓存（SoftJWTAuth 的 check，阶段7步骤1）
//	1 次读水位线
//	1 次取 id
//
// 每砍一次往返都是直接收益。于是把顺序调换过来，依据是一个观察：
//
//	**"取 id"这一趟本身就回答了"有没有热数据"。**
//
//	· 返回了东西   → 有热数据，水位线根本不需要知道
//	· 什么都没返回 → 要么 ZSET 是空的，要么请求纯粹落在冷区
//	                 （这两种情况都是少数，值得多花一次往返去分辨）
//
// 于是首页——最常见的那次请求——**只花 1 次往返**就结束了。
// 冷区的翻页没有变慢（还是 2 次），冷启动也是（多一次重建后的重取）。
//
// 这次调换有一个重要的性质：**两条分支的结果都是对的，选错只影响速度**。
// 因为冷路径那条 SQL（`WHERE create_time < 游标 ORDER BY create_time DESC`）
// 本身就是完整正确的答案 —— 冷热分离不是"两种语义"，而是
// "同一个答案的贵实现和便宜实现"。所以这个改动不需要新的正确性论证。
//
// ---------- 四条出口 ----------
//
//	① rediscache == nil          → 纯 MySQL（启动降级）
//	② 取 id 有结果               → 热路径（+ 冷热边界补齐）
//	③ 取 id 为空 + ZSET 不存在   → 自举重建，重取一次
//	④ 取 id 为空 + ZSET 存在     → 纯冷区，直接查库
//
// 任何一步 Redis 出错都**不会返回错误**，而是退回纯 MySQL 版 ——
// 这就是文档里"降级闭合"的意思：这条路走不通，一定有一条走得通的路。
func (f *FeedService) ListLatest(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error) {
	// ① 启动降级：整个缓存层没被启用
	if f.rediscache == nil {
		return f.listLatestFromDB(ctx, limit, latestBefore, viewerAccountID)
	}

	// ② 按游标从 ZSET 取 id —— 热路径唯一需要的东西。
	//
	// 首页（latestBefore 为零值）的 maxScore 是 "+inf"，即"最新的 limit 条"；
	// 翻页时是 (游标 - 1)。开闭区间的讲究全在 timelineIDs 里。
	//
	// 读失败（Redis 挂了/超时）→ 直接降级查库，**不重试**：
	// 主页是热路径，把 50ms 的失败拖成 3 次重试 = 150ms，
	// 比直接查一次库（~20ms）慢得多。降级不是"退而求其次"，
	// 在这个时间预算下它其实是更快的那个选择。
	ids, err := f.timelineIDs(ctx, limit, latestBefore)
	if err != nil {
		return f.listLatestFromDB(ctx, limit, latestBefore, viewerAccountID)
	}

	// ③ 取 id 为空 —— 现在才需要区分"ZSET 是空的"和"请求落在冷区"。
	//    这一步是新增的一次往返，但它只在冷启动和深翻页时发生。
	if len(ids) == 0 {
		exists, err := f.timelineExists(ctx)
		if err != nil {
			return f.listLatestFromDB(ctx, limit, latestBefore, viewerAccountID)
		}

		if !exists {
			// ZSET 不存在（首次部署 / 被 FLUSH / Redis 重启）→ 自举重建。
			//
			// 阶段9 之前，往 ZSET 里写数据的 TimelineMQ 消费者**还不存在**，
			// 所以这条路是常态。这也正是"没有 MQ 它也能工作"的实现方式：
			// 第一次请求自己建，阶段9 的消费者只是让"新发布的视频立刻进
			// ZSET"成为可能。
			rebuilt, err := f.rebuildTimeline(ctx)
			if err != nil {
				return f.listLatestFromDB(ctx, limit, latestBefore, viewerAccountID)
			}
			if !rebuilt {
				// 数据库里一条视频都没有 —— 空库不是错误，是正确答案。
				// 这个分支必须存在，否则重建会变成"每次都重建"。
				return ListLatestResponse{VideoList: []FeedVideoItem{}, HasMore: false}, nil
			}

			// 重建后重取一次。
			//
			// 注意这里**不是递归调用自己**（原项目的写法）：递归更短，
			// 但它有一个不受控的出口 —— 万一 ZAdd 静默失败（比如 rdb 被
			// 半初始化），递归就会在**请求路径上**无限自旋。
			// 多写三行把那个出口焊死是划算的。
			ids, err = f.timelineIDs(ctx, limit, latestBefore)
			if err != nil {
				return f.listLatestFromDB(ctx, limit, latestBefore, viewerAccountID)
			}
		}
	}

	// ④ 到这里 ids 还是空的，说明请求确实落在冷区（或者刚重建完仍然是空库）。
	var videos []*video.Video

	if len(ids) == 0 {
		videos, err = f.listLatestCold(ctx, limit, latestBefore)
		if err != nil {
			return ListLatestResponse{}, err
		}
	} else {
		videos, err = f.listLatestHot(ctx, ids, limit, latestBefore)
		if err != nil {
			return ListLatestResponse{}, err
		}
	}

	return f.assembleLatest(ctx, videos, limit, viewerAccountID)
}

// ListLikesCount 点赞榜（复合游标）
func (f *FeedService) ListLikesCount(ctx context.Context, limit int, cursor *LikesCountCursor, viewerAccountID uint) (ListLikesCountResponse, error) {
	videos, err := f.repo.ListLikesCountWithCursor(ctx, limit, cursor)
	if err != nil {
		return ListLikesCountResponse{}, err
	}
	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListLikesCountResponse{}, err
	}
	resp := ListLikesCountResponse{
		VideoList: feedVideos,
		HasMore:   len(videos) == limit,
	}
	// 书签 = 本页最后一条的 (点赞数, ID) 二元组，指针形式（omitempty）
	if len(videos) > 0 {
		last := videos[len(videos)-1]
		nextLikesCountBefore := last.LikesCount
		nextIDBefore := last.ID
		resp.NextLikesCountBefore = &nextLikesCountBefore
		resp.NextIDBefore = &nextIDBefore
	}
	return resp, nil
}

// ListByPopularity 热榜：**快照分页**（Redis 主路径）+ 三键游标（MySQL 降级）。
//
// ---------- 它解决的那个矛盾 ----------
//
// 实时榜每秒都在变。你翻第 1 页时 A 是第 1 名，翻第 2 页时 A 已经掉到第 5 名 ——
// 于是 A 被你看两遍，同时有几个视频你**永远刷不到**。名次互换这件事本身没错，
// 错的是"用两把不同的尺子量了同一次浏览"。
//
// 解法是把"用哪一版榜单"的决定权交给客户端：服务端返回 as_of（分钟对齐的时间戳），
// 客户端翻页时原样带回来，服务端用**同一份快照**按名次取区间。榜单就此冻结。
//
// ---------- 三层防抖，各掐一种抖动 ----------
//
//	① as_of 由客户端带回       掐"版本漂移"（不用服务端当前时间，用客户端开始浏览的那一版）
//	② 快照 key 按分钟命名      同一分钟的所有请求共享同一份 ZSET（配合 2min TTL）
//	③ ZREVRANGE 按 rank 取区间 在**不变的集合**上，offset 才是有意义的
//
// 注意 ② 和 ③ 是配套的：如果集合在变，按 rank 取第二页就是错的 ——
// 所以快照不是"性能优化"，它是分页正确性的**前提**。
//
// ---------- 双游标：降级是接口契约的一部分 ----------
//
// 响应**同时**携带两套游标：
//
//	as_of + next_offset                        Redis 版（as_of > 0 时有效）
//	next_latest_popularity/before/id           DB 版（每一页都附，取本页最后一条）
//
// 前端约定：as_of > 0 用 offset 翻页；一旦收到 as_of == 0，说明已降级，
// 此后改用三游标翻页。**每一页都下发 DB 游标**，是为了让"Redis 在第 N 页断掉"
// 这个事件在哪一页发生都不致命 —— 前端手里永远有一份能继续翻的 DB 书签。
//
// ---------- ⚠ 两个榜单不是同一份数据 ----------
//
//	Redis 版 = 近 60 分钟**净增量**（滑动窗口；会掉榜；可能是负数）
//	DB   版 = 历史**累计** popularity（单调；只增不减；无时间语义）
//
// 所以停 Redis 的那一瞬间，用户看到的"第一名"会换人，而且**这是正常的**。
// 这是已知取舍，不是 bug —— 原项目同样如此。想消掉这个差异，得让 MySQL 也维护
// 一份"近 60 分钟"的视图（小时桶预聚合之类），那是另一个量级的工程。
func (f *FeedService) ListByPopularity(ctx context.Context, limit int, reqAsOf int64, offset int, viewerAccountID uint, latestPopularity int64, latestBefore time.Time, latestIDBefore uint) (ListByPopularityResponse, error) {
	// offset 归一化。handler 校验了 limit 和三个 DB 游标，但**没管 offset** ——
	// 负数 offset 会让 ZREVRANGE 的语义变成"从榜尾倒数"（Redis 的负索引），
	// 静默返回一堆没人要的数据。在进入任何取数逻辑之前夹一次。
	if offset < 0 {
		offset = 0
	}

	// 唯一的 Redis 分支入口。cache == nil（启动时 Redis 就没连上）时整个跳过，
	// 和"运行中 Redis 报错"走的是同一个降级出口 —— 这就是文档说的
	// "降级的两种形态收敛到同一个查询"。
	if f.rediscache != nil {
		if resp, ok := f.listByPopularityFromSnapshot(ctx, limit, reqAsOf, offset, viewerAccountID); ok {
			return resp, nil
		}
	}

	return f.listByPopularityFromDB(ctx, limit, viewerAccountID, latestPopularity, latestBefore, latestIDBefore)
}

// listByPopularityFromSnapshot 热榜的 Redis 主路径。
//
// 返回值 ok == false 表示"这条路走不通，请去走 DB 降级"。注意它**不返回错误** ——
// 不是漏了，是刻意的：这一层里所有的失败（聚合失败、读取失败、空榜）
// 都是**可降级的**，没有一种失败值得把错误抛给用户。
// 错误在内部记日志（否则降级会变成静默的，运维看不出 Redis 出了问题）。
func (f *FeedService) listByPopularityFromSnapshot(ctx context.Context, limit int, reqAsOf int64, offset int, viewerAccountID uint) (ListByPopularityResponse, bool) {
	// ---------- ① as_of 定版（难点1 的"对齐函数唯一"）----------
	//
	// 两个分支的差别只有一个：**用谁的时间**。
	//
	//	reqAsOf > 0  客户端带回来的版本（翻第 2 页及以后）
	//	reqAsOf == 0 服务端当前时间（第 1 页，或前端第一次进榜）
	//
	// `time.Unix(reqAsOf, 0)` 得到的是本地时区的 Time，所以**必须**跟一个
	// `.UTC()` 才能和写路径的 key 对上。写路径（PopularityBucketKey）里
	// 也是先 UTC 再 Truncate，两处是同一个算法 —— 这是整个热榜最容易错、
	// 且错了完全不报错的地方（现象是"榜单永远空"）。
	//
	// Truncate 在这里其实是**冗余**的（reqAsOf 本来就由服务端分钟对齐后回传），
	// 但它挡住了一种客户端行为：把 as_of 自己改了（四舍五入、加了几秒）再送回来。
	// 多一次幂等操作换"客户端乱改也打不偏"，值。
	asOf := time.Now().UTC().Truncate(time.Minute)
	if reqAsOf > 0 {
		asOf = time.Unix(reqAsOf, 0).UTC().Truncate(time.Minute)
	}

	// 读路径**直接用请求的 ctx**，不套 WithoutCancel —— 和写路径相反，
	// 也是刻意的：客户端已经断开的话，这次读没有任何人在等，早点放弃更好
	// （还能省下 ZUNIONSTORE 的 CPU）。写路径不能这样，因为写的是"已经提交的
	// MySQL 变更的副作用"，它和请求的存活无关。
	opCtx, cancel := context.WithTimeout(ctx, popularityReadOpTimeout)
	defer cancel()

	dest := video.PopularitySnapshotKey(f.rediscache, asOf)

	// ---------- ② 快照：EXISTS 判断 → 缺了才重算（难点2/3）----------
	//
	// 快照把聚合成本**按分钟摊平**：从"每个请求一次 O(N*K)"变成
	// "每分钟一次 ZUNIONSTORE + N 次纯读"。
	//
	// `exists, _ :=` —— **有意吞掉错误**，这一行值得解释：
	// EXISTS 查询失败（Redis 抖动、超时）时把结果当 false 处理，于是往下走
	// 重算一次。重算的输入是同样的 60 个桶，ZUNIONSTORE 是幂等的，
	// 所以代价只是白烧一次 CPU，结果完全正确。
	// 反过来（当成 true 而不去重算）才是灾难：会读到一个不存在的快照 key，
	// ZREVRANGE 返回空，用户看到一个空榜。
	//
	// ⚠ EXISTS → ZUNIONSTORE → EXPIRE **不是原子**的：同一分钟内的并发首翻
	// 会重算两三次（每个请求都看到 exists=false）。结果幂等所以无害，
	// 只是浪费。原项目接受这个浪费换代码简单；要抠就上锁或 singleflight
	// （本服务已经有 requestGroup，但热榜这里刻意没用 —— 见文件尾的说明）。
	exists, _ := f.rediscache.Exists(opCtx, dest)
	if !exists {
		keys := video.PopularityWindowKeys(f.rediscache, asOf)

		// ZUNIONSTORE 的 dst 就是 dest，K 是 60 个桶，AGGREGATE SUM。
		// 60 个桶里大部分是**空的**（只有被互动过的分钟才有 key）——
		// 实测确认过：不存在的 key 被当作空集合，不报错。
		// 这带来一个必须知道的副作用：如果 as_of 太旧（超过 2h，桶已过期），
		// 聚合**不会失败**，而是静默地少算几个桶 —— 窗口实际变短、分数整体缩水。
		// 现象是"榜单看起来有点不对但说不清哪里不对"。
		// 正常翻页不会遇到（as_of 只旧 2 分钟），但客户端乱传旧 as_of 会遇到。
		if err := f.rediscache.ZUnionStore(opCtx, dest, keys, "SUM"); err != nil {
			log.Printf("[feed] 热榜快照聚合失败，降级 DB key=%s: %v", dest, err)
			return ListByPopularityResponse{}, false
		}

		// EXPIRE 必须紧跟，理由和桶那边一样（ZUNIONSTORE 隐式建的 key 不带 TTL），
		// 而且快照是内存里**最大**的一份数据（member 集合 = 60 分钟内所有被
		// 互动过的视频），忘设 TTL 等于每分钟泄漏一份榜单。
		// 这里也忽略错误：没设上 TTL 最坏是这份快照多活一会儿，不影响正确性。
		_ = f.rediscache.Expire(opCtx, dest, video.PopularitySnapshotTTL)
	}

	// ---------- ③ 按 rank 取一页 ----------
	//
	// ZREVRANGE：按 score **从大到小**取 [offset, offset+limit-1] 区间。
	// "从大到小"= 热度高的在前，和 SQL 的 `ORDER BY popularity DESC` 对应。
	//
	// 同分怎么办？ZSET 内部按 member 的字典序定序（实测：score 全为 5 的
	// 成员 7/10/9/100，ZREVRANGE 输出 9 7 100 10 —— 注意不是数值逆序）。
	// 这个"字典序"看起来随意，但它给了我们最需要的东西：**确定性**。
	// 只要 member 编码不变，同分视频的相对顺序就永远不变，
	// 翻页不会因为同分而抖 —— 而"稳定"正是整个快照机制存在的理由。
	members, err := f.rediscache.ZRevRange(opCtx, dest, int64(offset), int64(offset+limit-1))
	if err != nil {
		log.Printf("[feed] 热榜快照读取失败，降级 DB key=%s: %v", dest, err)
		return ListByPopularityResponse{}, false
	}

	// ---------- ④ 空结果的两个含义，必须分开处理（难点4）----------
	//
	// "取到空"有两种完全不同的原因，混起来的后果是给用户一个假空榜：
	//
	//	offset > 0  翻到底了。这是正常结果 —— 返回空列表 + has_more=false，
	//	            **as_of 照回**（前端据此知道"我还在 Redis 模式，只是翻完了"）
	//
	//	offset == 0 榜是空的。两种可能：冷启动（还没有任何互动进桶）、
	//	            或 as_of 太旧导致桶全过期。**这两种都应该去查 DB** ——
	//	            因为 DB 里可能有真实的视频，给一个空榜是错的。
	//
	// 这个分岔是"降级"和"翻到底"能被区分开的关键。漏了 offset 判断，
	// 用户翻到末尾会看到一个"刷新一下又有了"的诡异空列表。
	if len(members) == 0 {
		if offset > 0 {
			// 翻到底：NextOffset **原样返回**，不要再 +limit。
			// 语义是"你已经在末尾之外了"，继续 +limit 会让 next_offset
			// 越滚越大，前端如果按它显示"第 N 页"会显示出一个不存在的页码。
			return ListByPopularityResponse{
				VideoList:  []FeedVideoItem{},
				AsOf:       asOf.Unix(),
				NextOffset: offset,
				HasMore:    false,
			}, true
		}
		return ListByPopularityResponse{}, false
	}

	// ---------- ⑤ member → id（难点3 的"member 编码即协议"）----------
	//
	// ParseUint 失败或为 0 的 member **直接丢弃**，不报错。
	// 什么样的情况会走到这里：桶里混进了非数字的 member（历史遗留、
	// 别人手工调试留下的脏数据、或将来某次编码改动没清干净）。
	// 丢弃它们只会让榜单少一条，报错会让整个榜 500 —— 前者明显更好。
	ids := make([]uint, 0, len(members))
	for _, m := range members {
		id, err := strconv.ParseUint(m, 10, 64)
		if err != nil || id == 0 {
			continue
		}
		ids = append(ids, uint(id))
	}

	// ---------- ⑥ 取详情 + **按 rank 重排**（难点2 的"已删视频的洞"）----------
	//
	// 这里用 feed 自己的 repo.GetByIDs 直查 MySQL，**不走** GetVideoByIDs 的
	// 三级缓存（L1 go-cache → L2 Redis → L3 MySQL）。这是刻意的选择：
	//
	//	榜单页每次就 10 条，一条 `IN (...)` 查询足够，比三级缓存的两次
	//	Redis 往返 + 序列化开销更简单也更快。三级缓存是为"一次要 100 条
	//	且反复访问同一批"的时间线设计的，热榜的形状不一样。
	//
	// ⚠ `IN` 查询**不保证顺序**：MySQL 返回行的顺序取决于它自己（通常按主键），
	// 不是 IN 列表的顺序。所以下面必须重排 —— 忘了这一步，热榜会按视频 ID
	// 排序显示，看起来像"热度完全没生效"。
	videos, err := f.repo.GetByIDs(ctx, ids)
	if err != nil {
		// 这里是真·DB 错误。返回 false 让上层再走一次 repo.ListByPopularity，
		// 那次会失败并返回真正的错误给用户 —— 比在这里吞掉更好。
		return ListByPopularityResponse{}, false
	}
	ordered := orderVideosByIDs(ids, videos)

	items, err := f.buildFeedVideos(ctx, ordered, viewerAccountID)
	if err != nil {
		return ListByPopularityResponse{}, false
	}

	// NextOffset = offset + **len(items)**，不是 offset + limit。
	//
	// 两者在有"洞"时不同：桶里记着视频 X，但 X 已经从 MySQL 删了 →
	// GetByIDs 查不到 → ordered 短一条 → len(items) < limit。
	//
	// 用 len(items) 是个**已知取舍**：它按"实际返回了几条"推进，于是
	// 下一页的起点会**回退一格**，可能重复返回上一页的最后一条。
	// 用 limit 推进则相反 —— 跳过一条，用户永远看不到它。
	//
	// 选"重复"而不是"遗漏"，因为重复用户能看出来（"咦这条刚看过"），
	// 遗漏用户永远不知道。而且洞本身量很小（原项目没做死视频清理）。
	// 严谨做法是按 rank 推进（记住"我读到第几名"），或者定时 ZREM 掉
	// 已删视频 —— 都属于进阶改进。
	resp := ListByPopularityResponse{
		VideoList:  items,
		AsOf:       asOf.Unix(),
		NextOffset: offset + len(items),
		HasMore:    len(items) == limit,
	}

	// 双游标并存：Redis 版每一页都**顺带**下发 DB 三游标（取本页最后一条）。
	//
	// 这才是"降级对前端透明"的实现方式 —— 不是为了好看，是为了让
	// "Redis 恰好在你翻第 3 页时断了"这个事件不至于让用户重新开始翻。
	// 前端从第 1 页起就得把这三个值存下来，别只存 as_of/offset。
	//
	// 取 ordered 的最后一条（而不是 videos 的）：ordered 才是本页**真实返回**
	// 的那一批，videos 里可能含被剔除的、也可能顺序不同。
	if len(ordered) > 0 {
		last := ordered[len(ordered)-1]
		nextPopularity := last.Popularity
		nextBefore := last.CreateTime
		nextID := last.ID
		resp.NextLatestPopularity = &nextPopularity
		resp.NextLatestBefore = &nextBefore
		resp.NextLatestIDBefore = &nextID
	}
	return resp, true
}

// listByPopularityFromDB 热榜的 MySQL 降级路径：三键游标分页。
//
// 注意它和 Redis 版**不是同一份数据**（见 ListByPopularity 的说明）：
// 这里排的是 videos.popularity 累计值，没有时间窗口语义。
//
// 响应里 AsOf=0、NextOffset=0 —— 这两个 0 是给前端的**降级信号**：
// "别再按 offset 翻页了，改用我给你附的三游标"。所以这里必须把它们
// 显式置 0（结构体零值恰好就是 0，但别依赖这个巧合 —— 未来加字段时容易踩）。
func (f *FeedService) listByPopularityFromDB(ctx context.Context, limit int, viewerAccountID uint, latestPopularity int64, latestBefore time.Time, latestIDBefore uint) (ListByPopularityResponse, error) {
	videos, err := f.repo.ListByPopularity(ctx, limit, latestPopularity, latestBefore, latestIDBefore)
	if err != nil {
		return ListByPopularityResponse{}, err
	}
	items, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListByPopularityResponse{}, err
	}
	resp := ListByPopularityResponse{
		VideoList:  items,
		AsOf:       0,
		NextOffset: 0,
		HasMore:    len(items) == limit,
	}
	if len(videos) > 0 {
		last := videos[len(videos)-1]
		nextPopularity := last.Popularity
		nextBefore := last.CreateTime
		nextID := last.ID
		resp.NextLatestPopularity = &nextPopularity
		resp.NextLatestBefore = &nextBefore
		resp.NextLatestIDBefore = &nextID
	}
	return resp, nil
}

// orderVideosByIDs 把 MySQL 返回的视频按**给定的 id 顺序**重排。
//
// 为什么必须有这一步：`WHERE id IN (...)` 的结果顺序由 MySQL 决定，
// 通常就是主键序 —— 而热榜要的是**热度序**（那个顺序信息只存在于
// ZSET 的 rank 里，ids 就是它）。不重排的话，榜单会按视频 ID 显示。
//
// 用 map 而不是排序：ids 本身已经是目标顺序，只需要"按 ids 遍历、查表"，
// 是 O(N)。用 sort 再定义一个比较函数反而更慢也更绕。
//
// **查不到的 id 会被跳过**（视频已删）—— 见调用处关于这个"洞"的说明。
func orderVideosByIDs(ids []uint, videos []*video.Video) []*video.Video {
	byID := make(map[uint]*video.Video, len(videos))
	for _, v := range videos {
		byID[v.ID] = v
	}
	ordered := make([]*video.Video, 0, len(ids))
	for _, id := range ids {
		if v, ok := byID[id]; ok {
			ordered = append(ordered, v)
		}
	}
	return ordered
}

// ListByFollowing 关注流（阶段6）。协议和 ListLatest 完全一样，只有数据源不同：
// 一个查全表，一个先用子查询收窄到"我关注的人"。
//
// **游标单位：毫秒，和 ListLatest 一致。**
//
// 这里和原项目**不一致**，是刻意改的。原项目的 listByFollowing 请求收 `latest_time`
// 是 **Unix 秒**（`time.Unix(req.LatestTime, 0)`）、响应 `next_time` 也是秒
// （`CreateTime.Unix()`），而 listLatest 两个方向都是毫秒。同一个字段名、
// 同一种游标语义，两条流用了两个单位。
//
// 为什么不能照抄：这两个流的游标在**前端共用同一个 composable**（useFeedStream），
// 传出去再传回来是一模一样的代码路径。后端一个毫秒一个秒，前端就得为每条流记一个
// 单位常量，或者更糟 —— 在某处悄悄漏掉换算，于是翻页跳过或重复若干条，
// 而且**只在第二页之后才看得出来**（第一页不传游标，永远是对的）。
// 他的 feed/service.go 里已经有一处被这个单位混乱咬过的注释
// （"注意：这里是秒！和 latest 游标的毫秒不同（原项目的时间单位瑕疵）"）。
//
// 所以这里统一成毫秒：和 ListLatest 的请求、响应、以及 FeedVideoItem.CreateTime 的
// 上游值同一套。**唯一没统一的是 FeedVideoItem.CreateTime 本身（秒）** ——
// 那是给前端 formatTime(sec) 用的展示值，不是游标，两者本来就是两回事。
func (f *FeedService) ListByFollowing(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListByFollowingResponse, error) {
	// 回源逻辑抽成闭包：缓存未命中、抢不到锁、Redis 故障 —— 三条路最后都落到它。
	// 抽出来是为了让"从 DB 组装一个响应"只有一份实现，三条路不可能各自跑偏。
	fromDB := func() (ListByFollowingResponse, error) {
		videos, err := f.repo.ListByFollowing(ctx, limit, viewerAccountID, latestBefore)
		if err != nil {
			return ListByFollowingResponse{}, err
		}
		feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
		if err != nil {
			return ListByFollowingResponse{}, err
		}

		// 书签 = 本页最后一条的**毫秒**时间戳（和 ListLatest 一模一样）
		var nextTime int64
		if len(videos) > 0 {
			nextTime = videos[len(videos)-1].CreateTime.UnixMilli()
		}

		return ListByFollowingResponse{
			VideoList: feedVideos,
			NextTime:  nextTime,
			HasMore:   len(videos) == limit, // 满页才认为可能还有下一页
		}, nil
	}

	// ---------- 什么时候不缓存 ----------
	//
	// ① viewer == 0：没有"关注的人"这个概念。而且注意 repo 的行为 ——
	//    viewer==0 时它**完全不过滤**，返回的是全站最新流（见 repo.ListByFollowing
	//    的长注释）。这种请求本来就不该存在（handler 已经 401 挡住了），
	//    真漏进来也绝不能进缓存：key 里没有 viewer 就会和别人的页面串号。
	// ② cache == nil：启动降级。
	if viewerAccountID == 0 || f.rediscache == nil {
		return fromDB()
	}

	// ---------- 缓存 key ----------
	//
	// 三个分量缺一不可：limit（页大小）、accountID（**这个响应含 is_liked，
	// 是因人而异的**）、before（游标 —— 每一页是独立的一份缓存）。
	//
	// **这里用的是毫秒，不是文档写的秒。** 文档说"key 里 before 用秒"是照抄
	// 原项目，而原项目的游标恰好是秒精度（time.Unix）。本项目的游标在
	// handler 里就是 time.UnixMilli 解出来的（见 handler.go 的说明），
	// 同一个"秒"里可以有 1000 个不同的游标。用秒做 key = 这些游标共用一份缓存
	// → 翻页时**返回错误的一页**。凡是要拿游标进 key 的地方，精度必须和游标本身对齐。
	before := int64(0)
	if !latestBefore.IsZero() {
		before = latestBefore.UnixMilli()
	}
	cacheKey := f.rediscache.Key(
		"feed:listByFollowing:limit=%d:accountID=%d:before=%d",
		limit, viewerAccountID, before,
	)

	// hit 是给"双重检查"和"轮询"用的读缓存：只关心"拿到了一个能用的结果吗"。
	// 首次读**不用**它 —— 见下面那段 switch 的说明。
	hit := func() (ListByFollowingResponse, bool) {
		opCtx, cancel := context.WithTimeout(ctx, feedCacheOpTimeout)
		defer cancel()
		b, err := f.rediscache.GetBytes(opCtx, cacheKey)
		if err != nil {
			return ListByFollowingResponse{}, false
		}
		var cached ListByFollowingResponse
		if err := json.Unmarshal(b, &cached); err != nil {
			return ListByFollowingResponse{}, false
		}
		return cached, true
	}

	// ---------- ① 读缓存 ----------
	//
	// 这里是**一次读**就把三种情况分清的：命中 / 真未命中 / Redis 故障。
	// 对比 video 详情那版（GetDetail）—— 那边读了两遍，因为它的 getCached 闭包
	// 把所有 error 都吞成"没命中"，吞掉之后就没法区分"key 不在"和"Redis 挂了"，
	// 只好再裸读一次做裁决。这里不吞 error，直接拿原始 err 判断，一次就够。
	//
	// 少一次往返的意义不只是省 1ms：两次读之间存在窗口，第二次读到的状态
	// 可能和第一次不一致（别人刚好回填了），逻辑分支会变得很难推演。
	firstCtx, cancelFirst := context.WithTimeout(ctx, feedCacheOpTimeout)
	raw, firstErr := f.rediscache.GetBytes(firstCtx, cacheKey)
	cancelFirst()

	switch {
	case firstErr == nil:
		var cached ListByFollowingResponse
		if err := json.Unmarshal(raw, &cached); err == nil {
			return cached, nil
		}
		// 能读到但 unmarshal 失败 = 缓存里是脏数据（格式变了、写坏了）。
		// 不当故障处理，而是**当未命中**往下走：抢锁 → 回源 → 回填会把它覆盖掉。
		// 这也是缓存里唯一能自我修复的地方
	case !rediscache.IsMiss(firstErr):
		// Redis 故障（超时/拒连/权限）：抢锁也会失败，直接回源
		return fromDB()
	}

	// ---------- ② 真未命中 → 抢防击穿锁 ----------
	lockKey := "lock:" + cacheKey
	lockCtx, cancelLock := context.WithTimeout(ctx, feedCacheOpTimeout)
	token, locked, err := f.rediscache.Lock(lockCtx, lockKey, followingLockTTL)
	cancelLock()
	if err != nil {
		return fromDB()
	}

	if locked {
		// 用 Background 释放：请求可能已经结束，锁还是必须放掉，
		// 否则要等满 500ms TTL 期间所有并发请求都在空转
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), feedCacheOpTimeout)
			defer cancel()
			_ = f.rediscache.Unlock(unlockCtx, lockKey, token)
		}()

		// 双重检查：从"读到 miss"到"抢到锁"之间有窗口期，前一个持锁者可能刚回填完
		if cached, ok := hit(); ok {
			return cached, nil
		}

		resp, err := fromDB()
		if err != nil {
			return ListByFollowingResponse{}, err
		}
		// 回填。用 WithoutCancel：请求 ctx 可能已经因为客户端断开而取消，
		// 但"查询已完成、把结果存起来"值得做完 —— 下一个请求就能命中
		setCtx, cancelSet := context.WithTimeout(context.WithoutCancel(ctx), feedCacheOpTimeout)
		defer cancelSet()
		if b, err := json.Marshal(resp); err == nil {
			if err := f.rediscache.SetBytes(setCtx, cacheKey, b, f.cacheTTL); err != nil {
				log.Printf("[feed] 回填关注流缓存失败 account=%d: %v", viewerAccountID, err)
			}
		}
		return resp, nil
	}

	// ---------- ③ 没抢到锁：别人正在回源，轮询等一下 ----------
	for i := 0; i < followingLockWaitRounds; i++ {
		select {
		case <-ctx.Done():
			return ListByFollowingResponse{}, ctx.Err()
		case <-time.After(followingLockWaitStep):
		}
		if cached, ok := hit(); ok {
			return cached, nil
		}
	}

	// ---------- ④ 等不到（持锁者挂了 / DB 特别慢）：自己兜底 ----------
	return fromDB()
}

// ListByTag 标签流
func (f *FeedService) ListByTag(ctx context.Context, tagName string, limit int, viewerAccountID uint) ([]FeedVideoItem, error) {
	videos, err := f.repo.ListByTag(ctx, tagName, limit)
	if err != nil {
		return nil, err
	}
	return f.buildFeedVideos(ctx, videos, viewerAccountID)
}

// Search 混合检索：两路召回 → RRF 融合 → 取详情 → 组装。
//
// 入口放在 feed 包而不是 search 包，唯一的理由就是下面这一步：
// **搜索结果必须是 FeedVideoItem**（带 is_liked、带 author）。
// 在 search 包里组装的话，is_liked 的批量查询、匿名降级、
// nil 切片兜底都得再写一遍 —— 而这个项目所有流的出口都收敛在 buildFeedVideos，
// 搜索没有理由成为例外。
func (f *FeedService) Search(ctx context.Context, q search.Query, cur *search.Cursor, limit int, viewerAccountID uint) (SearchResponse, error) {
	if f.searchSvc == nil {
		// 漏接线时**报错**而不是返回空列表：返回空列表的话，
		// 搜索页会显示"没有找到相关视频"，让人以为是语料问题
		return SearchResponse{}, errors.New("搜索服务未启用")
	}

	res, nextCursor, err := f.searchSvc.Search(ctx, q, cur, limit)
	if err != nil {
		return SearchResponse{}, err
	}

	items, err := f.hydrateInOrder(ctx, res.IDs, viewerAccountID)
	if err != nil {
		return SearchResponse{}, err
	}

	return SearchResponse{
		VideoList:  items,
		NextCursor: nextCursor,
		HasMore:    nextCursor != "",
		Mode:       res.Mode,
		Arms:       res.Arms,
		Total:      res.Total,
	}, nil
}

// hydrateInOrder 按给定顺序取视频详情并组装。**搜索最容易出的 bug 就在这里**。
//
// GetByIDs 用的是 `WHERE id IN ?`，它返回的是 **MySQL 觉得方便的**顺序
// （通常是主键顺序），**不是融合顺序**。所以必须自己重排一次。
//
// 不重排的后果特别隐蔽：相关度排序会静默退化成一团乱序，而且
// ——不报错、不返回空、页面上有内容、只是"排序看起来不太准"。
// 没有人会为"排序不太准"去查代码。这一条和 feed/repo.go 顶部那句
// "排序键和 WHERE 游标键必须是同一组"是同一类纪律的两个面。
//
// 顺带说明为什么用 map 而不是保持两个切片对齐：**GetByIDs 可能少返回**。
// 冻结列表是第 1 页算出来的快照，翻到第 3 页时其中某条视频可能已经被删了 ——
// 那时它查不出来，只能丢掉（而不是填一个空洞）。表现是这一页少一条，
// 属于快照语义的固有代价。
func (f *FeedService) hydrateInOrder(ctx context.Context, ids []uint, viewerAccountID uint) ([]FeedVideoItem, error) {
	videos, err := f.repo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[uint]*video.Video, len(videos))
	for _, v := range videos {
		byID[v.ID] = v
	}
	ordered := make([]*video.Video, 0, len(videos))
	for _, id := range ids {
		if v, ok := byID[id]; ok {
			ordered = append(ordered, v)
		}
	}
	return f.buildFeedVideos(ctx, ordered, viewerAccountID)
}

// buildFeedVideos 组装器：把 video.Video 列表翻译成前端要的 FeedVideoItem 列表。
// 所有 Feed 接口共用这一个出口，is_liked 的接入点也在这里
//
// 不 nil-guard f.likeRepo：漏接线时应该当场 panic，而不是静默退回"is_liked 恒 false"
// —— 那正是阶段3 的旧行为，会让人以为功能没坏（"我明明赞了，怎么还是空心"）。
func (f *FeedService) buildFeedVideos(ctx context.Context, videos []*video.Video, viewerAccountID uint) ([]FeedVideoItem, error) {
	// 阶段3 留的口子在这里堵上：先收齐本页的 video id，**一次 IN 查**把这批视频
	// 的"我赞过没有"全部拿回来。逐条调 IsLiked 就是 N+1 —— 一页几十条要几十次往返。
	//
	// 匿名/未登录（viewerAccountID == 0）时 repo 直接返回空 map，
	// 取值天然落成 false，所以这里**不需要**为游客写任何分支
	ids := make([]uint, 0, len(videos))
	for _, v := range videos {
		ids = append(ids, v.ID)
	}
	likedMap, err := f.likeRepo.BatchGetLiked(ctx, ids, viewerAccountID)
	if err != nil {
		return nil, err
	}

	feedVideos := make([]FeedVideoItem, 0, len(videos))
	for _, video := range videos {
		feedVideos = append(feedVideos, FeedVideoItem{
			ID:          video.ID,
			Author:      FeedAuthor{ID: video.AuthorID, Username: video.Username},
			Title:       video.Title,
			Description: video.Description,
			PlayURL:     video.PlayURL,
			CoverURL:    video.CoverURL,
			CreateTime:  video.CreateTime.Unix(), // 注意：这里是秒！和 latest 游标的毫秒不同（原项目的时间单位瑕疵）
			LikesCount:  video.LikesCount,
			// 取值不需要判"在不在 map 里"：BatchGetLiked 返回的是非 nil map，
			// 没赞过的 key 不存在 → 零值 false，正好就是我要的答案
			IsLiked: likedMap[video.ID],
		})
	}
	return feedVideos, nil
}
