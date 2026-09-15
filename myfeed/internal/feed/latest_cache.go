package feed

import (
	"context"
	"log"
	"strconv"
	"time"

	"myfeed/internal/video"

	redis "github.com/redis/go-redis/v9"
)

// 阶段7：全局时间线（冷热分离）的参数。
//
// 唯一一个需要解释的数：**timelineSize**
//
//	1000 条不是拍脑袋 —— 它是"内存占用"和"冷热边界命中率"的乘积结果：
//	  · 热区越大 → 冷热边界越靠后 → 更多请求能走热路径（省一次 MySQL）
//	  · 热区越大 → ZSET 常驻内存越多，且重建时那一次 MySQL 捞得越多
//	一条 ZSET 成员 = member(视频 id 字符串 ≈ 6 字节) + score(8 字节) +
//	Redis 自身的 dict/skiplist 开销，实测约 100 字节/条。
//	1000 条 ≈ 100KB —— 单 key 100KB 是安全的；10 万条 ≈ 10MB 就偏重了
//	（备份/RDB 重写/主从全量同步时都要搬这 10MB）。
//
//	更关键的是：**主页第一屏只需要 10 条**。1000 条意味着前 100 页
//	全部落在热区 —— 而真实流量里几乎没有人翻过第 5 页。所以再往上加
//	（比如 10 万）换来的命中率提升接近于零，内存却是 100 倍。
const (
	timelineSize = 1000

	// rebuildTimeout 自举重建的超时。给 2 秒（远宽于其他 Redis 操作的 50ms）：
	// 这一次要 ZAdd 1000 个成员，还要等一次 LIMIT 1000 的 MySQL 查询。
	// 但它前面有 singleflight 挡着，全局只会有一个请求付这个代价。
	rebuildTimeout = 2 * time.Second
)

// timelineKey 全局时间线的 Redis key。
//
// 用方法而不是常量，是为了让 keyPrefix（v1:）只有 rediscache.Key() 一个来源。
// 写成常量字符串的话，一旦哪天前缀策略变了，这个 key 会静默地不合群 ——
// 而它的症状是"时间线缓存永远不命中"，非常难查。
func (f *FeedService) timelineKey() string {
	return f.rediscache.Key("feed:global_timeline")
}

// rebuildTimeline 从 MySQL 捞最近 timelineSize 条，灌进 ZSET。
//
// 返回值 rebuilt 的语义是"ZSET 现在有数据了吗"，**不是**"重建成功了吗"：
//
//	rebuilt=false, err=nil  → 数据库是空的（正常情况，不该无限重试）
//	rebuilt=false, err!=nil → 重建失败（调用方降级查库）
//	rebuilt=true            → 灌好了，可以往下走
//
// 整个重建过程在 singleflight 里 —— 这是本文件里**最重要**的一处合并：
// 空 ZSET 的那一瞬间（第一次部署、Redis 重启后）会有成千上万个请求
// 同时发现它是空的。没有合并的话，它们会各自发起一次 LIMIT 1000 的查询，
// 然后各自 ZAdd 1000 个成员 —— 一次冷启动就是几千次全表扫。
// 有合并，全局只有 1 次。
//
// 注意合并键**不分用户、不分游标**（sf:fallback:...）：重建的结果对所有人
// 都一样，所以故意用一个全局键把所有人合并到一起。这和冷路径的
// "按游标分锁"是相反的取舍，理由在各自的注释里。
func (f *FeedService) rebuildTimeline(ctx context.Context) (bool, error) {
	sfKey := f.rediscache.Key("sf:fallback:global_timeline_rebuild")

	v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
		// 无视游标，直接捞最新的一批。time.Time{} 是零值 = 第一页。
		dbVideos, err := f.repo.ListLatest(ctx, timelineSize, time.Time{})
		if err != nil {
			return nil, err
		}
		if len(dbVideos) == 0 {
			return false, nil
		}

		// 用独立 ctx（Background + 2s）而不是请求 ctx：
		// 这是"重建全世界共用的一个 key"，不该被某一个用户的断开打断。
		bgCtx, cancel := context.WithTimeout(context.Background(), rebuildTimeout)
		defer cancel()

		elems := make([]redis.Z, 0, len(dbVideos))
		for _, v := range dbVideos {
			elems = append(elems, redis.Z{
				// score 必须是**毫秒**，且必须和游标同单位。
				// 文档的 key 表里写的是 "createTime 毫秒"，本项目的游标
				// 也确实来自 time.UnixMilli —— 两边对齐了。
				// 如果一边用秒一边用毫秒，水位的数量级会差 1000 倍，
				// 表现为"所有请求都走冷路径"（热区形同虚设），
				// 而不会有任何报错。
				Score:  float64(v.CreateTime.UnixMilli()),
				Member: strconv.FormatUint(uint64(v.ID), 10),
			})
		}

		key := f.timelineKey()
		if err := f.rediscache.ZAdd(bgCtx, key, elems...); err != nil {
			return nil, err
		}

		// 只留最近 timelineSize 条。
		//
		// 本次重建刚好只捞了 timelineSize 条，所以这两行现在是空操作 ——
		// 但阶段9 的 TimelineMQ 消费者会持续 ZAdd 新视频，那时候**必须**
		// 有东西裁掉老的，否则 ZSET 会无限增长（正好抵消掉冷热分离的全部意义）。
		// 现在就写上去，是因为裁剪逻辑属于"ZSET 的写入方"，
		// 而这里是阶段7 唯一的写入方；等到阶段9 再补，很容易补在消费者那边
		// 而漏掉重建这条路径。
		//
		// stop = -(timelineSize+1)：ZRemRangeByRank(0, -1001) 删掉
		// 排名 0 到（总数-1001）的所有成员，留下最后 1000 个。
		if err := f.rediscache.ZRemRangeByRank(bgCtx, key, 0, -(timelineSize + 1)); err != nil {
			// 裁剪失败不是致命错误：ZSET 里只是多了些旧数据，
			// 下次重建还会再裁一次。所以这里只记日志、不返回错误。
			log.Printf("[feed] 时间线 ZSET 裁剪失败: %v", err)
		}
		return true, nil
	})

	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

// listLatestCold 冷路径：直接查 MySQL。
//
// ---------- 为什么这里也用 singleflight，而且按**游标**分锁 ----------
//
// 冷区流量天然很小，但它有一个很不好的形状：**同一个游标会被反复请求**。
// 想象一个爬虫从第 1 页顺序往下翻到第 500 页，或者一个用户按住下拉不放 ——
// 热区的请求被 L1/L2 挡住了，全是"同一个游标"的冷请求直接打在 MySQL 上。
//
// 按游标分锁（limit + 游标时间戳）而不是一个全局键：冷区的不同游标之间
// 没有任何共享，用全局键会把互不相干的翻页请求串成一条链，
// 第 500 页的慢查询会拖住第 60 页的快查询。
//
// ---------- 为什么不回写 ZSET ----------
//
// 这是冷热分离里最容易写错的一行。冷查询的结果是"第 500 页的老视频"，
// 如果把它们也 ZAdd 进 feed:global_timeline，会发生两件事：
//
//	① 热点时间线里混进一堆没人看的老古董
//	② ZSet 按 score 排序 + 只留 1000 条 —— 这些老古董 score 极小，
//	   会被下一次 ZRemRangeByRank 立刻裁掉，等于白写
//	   （但如果忘了裁剪，它们就会长期占据名额，把真正的新视频挤出去）
//
// 所以冷路径**只读不写**。热点时间线永远是"最近发布的 1000 条"，
// 语义干净。
func (f *FeedService) listLatestCold(ctx context.Context, limit int, latestBefore time.Time) ([]*video.Video, error) {
	// 零值游标的 UnixMilli() 是个很大的负数 —— 拿它当 key 的一部分没问题，
	// 因为"首页的冷查询"本身也是一个该被合并的独立游标。
	sfKey := f.rediscache.Key("sf:cold:listLatest:%d:%d", limit, latestBefore.UnixMilli())

	v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
		return f.repo.ListLatest(ctx, limit, latestBefore)
	})
	if err != nil {
		return nil, err
	}
	return v.([]*video.Video), nil
}

// listLatestHot 热路径：把 ZSET 给出的 id 还原成视频，冷了再补齐。
//
// ids 由调用方（ListLatest）取好传进来，而不是在这里查 ZSET ——
// 因为调用方**需要用"取值是否为空"来决定后面走哪条路**，
// 而那个信息只有取 id 的那一次往返才知道。
// 让这个函数自己查 ZSET 的话，调用方就得再查一次才能做同样的判断，
// 等于把刚省下来的那次往返又还回去。
func (f *FeedService) listLatestHot(ctx context.Context, ids []uint, limit int, latestBefore time.Time) ([]*video.Video, error) {
	videos, err := f.GetVideoByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	// ---------- 击穿冷热边界：热区凑不满 limit，用冷查询补齐 ----------
	//
	// 三种情况会走到这里，而且都很常见：
	//	① 翻到了热区的最底部（ZSET 里剩下的不足 limit 条）
	//	② 热区里有视频被删了（GetVideoByIDs 把它们丢掉了，见它的注释）
	//	③ ZSET 里的 id 有脏数据
	// 所以这不是"边界情况"，是每次翻到第 100 页附近都会发生的主路径。
	if len(videos) >= limit {
		return videos, nil
	}
	remain := limit - len(videos)

	// 冷查询的游标 = 热区**最后一条**（最老的那条，因为 ZSET 是倒序取的）。
	// 这样冷查询拿到的全部严格老于它，两段拼接既不重也不漏。
	coldCursor := latestBefore
	if len(videos) > 0 {
		coldCursor = videos[len(videos)-1].CreateTime
	}

	cold, err := f.listLatestCold(ctx, remain, coldCursor)
	if err != nil {
		// 补齐失败**不该让整页失败**：手上已经拿到的热数据是一页有效的结果，
		// 只是短了几个。返回一页短的，比返回 500 好 ——
		// 用户看到"这一页只有 7 条"，比"主页报错了"轻得多。
		log.Printf("[feed] 冷热边界补齐失败（返回部分页）: %v", err)
		return videos, nil
	}
	return append(videos, cold...), nil
}

// timelineIDs 从 ZSET 按游标倒序取最多 limit 个 id。
//
// 这是热路径的**第一次、也常常是唯一一次** Redis 往返。
func (f *FeedService) timelineIDs(ctx context.Context, limit int, latestBefore time.Time) ([]uint, error) {
	// maxScore 的构造是这里最容易错的一行。
	//
	// 首页（latestBefore 为零值）用 "+inf"：不限上界，取最新的。
	// 翻页时用 (游标 - 1)，而不是游标本身 —— 因为 ZRangeBy 的 Max 是**闭区间**，
	// 写游标本身会把"上一页最后一条"（score 正好等于游标）再返回一次，
	// 表现为**每一页的第一条都是上一页的最后一条**。
	//
	// 这里 -1 是"毫秒减一"，等价于 SQL 里的 `create_time < ?`。
	// 两边必须是同一个开闭区间，否则翻页会在边界上重一条或漏一条 ——
	// 而这类 bug 只出现在"游标刚好落在某条视频的时间戳上"的时候，
	// 也就是只在翻页翻到边界时偶发，功能测试基本撞不到。
	maxScore := "+inf"
	if !latestBefore.IsZero() {
		maxScore = strconv.FormatInt(latestBefore.UnixMilli()-1, 10)
	}

	idStrs, err := f.rediscache.ZRevRangeByScore(ctx, f.timelineKey(), maxScore, "-inf", 0, int64(limit))
	if err != nil {
		return nil, err
	}

	ids := make([]uint, 0, len(idStrs))
	for _, s := range idStrs {
		id, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			// 单个 member 解析不出来就跳过，不中断整页。
			// 理论上不会发生（写入方一直用 FormatUint），
			// 但真发生了（比如有人手动 redis-cli ZADD 了个脏 member），
			// 让整页 500 是过度的反应。
			continue
		}
		ids = append(ids, uint(id))
	}
	return ids, nil
}

// timelineExists 判断时间线 ZSET 这个 key 存不存在。
//
// 只在"取 id 返回了空"那条分支上调用 —— 用途是把两种**结果相同但原因不同**
// 的情况分开：
//
//	key 不存在 → ZSET 从没建过（或刚被 FLUSH / Redis 重启丢了）→ 该自举重建
//	key 存在   → ZSET 好着呢，只是这个游标本身就落在冷区 → 直接查库，别重建
//
// 分不清这两者会怎样：把"纯冷区翻页"误判成"ZSET 空了"，
// 于是每一个深翻页的请求都触发一次自举重建（还要抢 singleflight），
// 白白捞 1000 条数据灌一个根本不缺数据的 ZSET。
// 反过来把"ZSET 空了"误判成冷区，则时间线永远不会变热 —— 主页每请求都查库。
func (f *FeedService) timelineExists(ctx context.Context) (bool, error) {
	return f.rediscache.Exists(ctx, f.timelineKey())
}

// listLatestFromDB 纯 MySQL 版（阶段7 之前的实现原样保留）。
//
// 它是这一整套的**兜底出口**，被调用的地方有五处：
// rediscache==nil、水位读取失败、重建失败、重建后 ZSET 仍空、热区 ZRevRange 失败。
// 保留它的意义不只是"以防万一" —— 它是这条路的**基准线**：
// 上面所有分支的正确性判断，最终都是"结果和它一致吗"。
func (f *FeedService) listLatestFromDB(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error) {
	videos, err := f.repo.ListLatest(ctx, limit, latestBefore)
	if err != nil {
		return ListLatestResponse{}, err
	}
	return f.assembleLatest(ctx, videos, limit, viewerAccountID)
}

// assembleLatest 出口组装：算游标、算 hasMore、翻译成 FeedVideoItem。
//
// 冷路径、热路径、纯 DB 路径**共用这一个出口**，是刻意的：
// hasMore 和 nextTime 的算法只要有两份实现，就一定会有一天不一致，
// 而那天的症状是"某一条路径翻页会卡住"。
func (f *FeedService) assembleLatest(ctx context.Context, videos []*video.Video, limit int, viewerAccountID uint) (ListLatestResponse, error) {
	// 书签 = 本页最后一条的毫秒时间戳（客户端下一页原样带回来）
	var nextTime int64
	if len(videos) > 0 {
		nextTime = videos[len(videos)-1].CreateTime.UnixMilli()
	}

	// 满页才认为可能还有下一页（近似法：总数恰为 limit 整数倍时，
	// 最后一页会多返回一次空页——原项目接受这个取舍）
	hasMore := len(videos) == limit

	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListLatestResponse{}, err
	}

	return ListLatestResponse{
		VideoList: feedVideos,
		NextTime:  nextTime,
		HasMore:   hasMore,
	}, nil
}
