package feed

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"myfeed/internal/video"
)

// 阶段7：Feed 实体三级缓存的参数。
//
// 注意这里的 TTL 是**三代并存**的，各自的量级差三个数量级，不是笔误：
//
//	L1 go-cache  5s    —— 进程内存，故意短。它只是"这一瞬间被疯抢的那几条"
//	                     的挡板（热点内容在秒级窗口内被反复请求）。
//	                     长了会出问题：多实例部署时，A 实例改了数据，
//	                     自己的 L1 自己删得掉，B 实例的 L1 只能等过期。
//	                      5s 就是"宁可多查几次 Redis，也不要长时间不一致"。
//	L2 Redis     1h    —— 跨实例共享，所以能被显式失效（UpdatePopularityCache
//	                      会 DEL 它），TTL 设长只影响"漏删时最坏脏多久"。
//	L3 MySQL     ∞     —— 数据的真相，没有缓存的概念。
//
// 这三层不是"缓存越大越好"的堆叠，而是**一致性半径**的三级：
// L1 只对自己负责（5s）、L2 对全体实例负责（1h + 显式失效）、L3 是唯一真相。
const (
	// entityCacheTTL 是 L2 的 TTL
	entityCacheTTL = time.Hour

	// l1EntryTTL 是 L1 单条的生命周期；l1SweepInterval 是 go-cache 内部
	// 清理 goroutine 的扫描周期。两者取同一个值是有意的：
	//
	// 原项目写的是 cache.New(3s, 5s) —— 默认过期 3s、扫描 5s，
	// 但它每次 Set 又显式传 5s，于是那个 3s 永远不会生效，只是个
	// 读代码时会让人停下来想一下的假旋钮。这里把默认值和显式值统一成
	// 同一个常量：Set 处仍然显式传（写 TTL 的地方就该看得见 TTL），
	// 但默认值不再是第二个没人用的数字。
	l1EntryTTL      = 5 * time.Second
	l1SweepInterval = 5 * time.Second

	// entityMGetTimeout L2 批量读的超时。给得比单条读（50ms）一样：
	// MGet 是**一次**往返，一次拿 N 条，所以不该因为它拿了 N 条就放大超时。
	entityMGetTimeout = 50 * time.Millisecond

	// entitySetTimeout L2 异步回写的超时
	entitySetTimeout = 50 * time.Millisecond
)

// GetVideoByIDs 批量取视频：L1 本地 → L2 Redis → L3 MySQL。
//
// 这是关注流/热榜/标签流/时间线的**共同底座** —— 那些流的 Redis 结构里
// 只存 video id（ZSET 的 member、List 的元素），把 id 还原成完整视频
// 全部要经过这里。所以它的性价比是整个阶段7 最高的一处：
// 优化它一个函数，四条流同时受益。
//
// ---------- 三级各自的职责 ----------
//
//	L1 挡的是**同一进程内、几秒内重复的同一批 id**（分页翻回来、多用户刷同一页）
//	L2 挡的是**跨请求、跨实例的重复**，一次 MGet 拿一整页，1 次往返
//	L3 是回源，但**并发被 singleflight 合并**：100 个 goroutine 要同一条 id，
//	   只有 1 个真的去查库
//
// ---------- 和 SETNX 防击穿锁的关系 ----------
//
// 两者都在防"并发回源"，但管的范围不同，**必须并存**：
//
//	singleflight  管**进程内**：同进程 100 个 goroutine → 1 次查库
//	SETNX+Lua 锁  管**跨进程**：10 个实例 → 1 个实例查库
//
// 只有 singleflight，10 个实例还是会打 10 次库；只有 SETNX，
// 单实例内 100 个 goroutine 会先抢锁再排队，多绕一圈 Redis。
// 视频详情用的是 SETNX 那套，这里用 singleflight —— 不是风格不统一，
// 而是**批量场景下的成本结构不同**（见 L3 段落的注释）。
func (f *FeedService) GetVideoByIDs(ctx context.Context, videoIDs []uint) ([]*video.Video, error) {
	if len(videoIDs) == 0 {
		return []*video.Video{}, nil
	}

	// rediscache == nil 的语义是"这个进程从来没被给过缓存"（启动降级），
	// 不是"缓存挂了"。所以这里**连 L1 一起关掉**。
	//
	// 为什么不"至少留着 L1"：L1 是纯进程内的，确实不依赖 Redis，
	// 留着它看起来更"聪明"。但那样一来 A/B 对照的两个分组都会带上 L1 的
	// 收益，"有 Redis vs 没 Redis"就量不出 Redis 到底值多少了 ——
	// 一个让对照实验失效的小聪明，比老老实实降级更亏。
	// 和 ListLatest 开头的判断是同一条纪律。
	if f.rediscache == nil {
		return f.repo.GetByIDs(ctx, videoIDs)
	}

	videoMap := make(map[uint]*video.Video, len(videoIDs))

	// ---------------------------------------------------------------
	// L1：进程内 go-cache
	// ---------------------------------------------------------------
	// 键**复用 Redis 的完整 key 字符串**（含 v1: 前缀），不另起一套命名。
	// 这样 L1 和 L2 的键在日志/调试里是同一条字符串，排查"缓存怎么没生效"
	// 时不需要在两套命名之间做心算。
	var missedL1 []uint
	for _, id := range videoIDs {
		key := f.rediscache.Key("video:entity:%d", id)
		v, found := f.localcache.Get(key)
		if !found {
			missedL1 = append(missedL1, id)
			continue
		}
		// 存的是**值**不是指针（见下面 Set 的地方），所以断言出来的是副本，
		// 取地址安全 —— 多个 goroutine 拿到的是各自独立的结构体，
		// 不存在"共享一个 *video.Video，有人改了大家都知道"的隐患。
		copied, ok := v.(video.Video)
		if !ok {
			missedL1 = append(missedL1, id)
			continue
		}
		videoMap[id] = &copied
	}

	if len(missedL1) == 0 {
		return buildOrderedResult(videoIDs, videoMap), nil
	}

	// ---------------------------------------------------------------
	// L2：Redis MGet（整批一次往返）
	// ---------------------------------------------------------------
	cacheKeys := make([]string, len(missedL1))
	for i, id := range missedL1 {
		cacheKeys[i] = f.rediscache.Key("video:entity:%d", id)
	}

	var missedL2 []uint

	mgetCtx, cancelMGet := context.WithTimeout(ctx, entityMGetTimeout)
	results, err := f.rediscache.MGet(mgetCtx, cacheKeys...)
	cancelMGet()

	if err != nil {
		// Redis 挂了或超时 → **整批**降级到 L3，不做逐条判断。
		//
		// 为什么是整批而不是逐条：MGet 失败时 results 可能是 nil，
		// 逐条检查会"全部未命中"，得到和整批降级一样的结论，只是多写一遍
		// 循环。而如果把"results 短于 cacheKeys"当成部分成功，就要处理
		// 下标错位 —— MGet 的返回值是**按下标对应**的，短了后面全错位。
		// 一次往返的批量读没有"部分成功"这个概念，认了更安全。
		log.Printf("[feed] L2 MGet 失败，整批降级到 MySQL: %v", err)
		missedL2 = missedL1
	} else {
		for i, res := range results {
			if i >= len(missedL1) {
				// go-redis 正常不会返回更长的切片，但不做这个防御的话
				// 越界会 panic 在请求路径上。
				break
			}
			id := missedL1[i]

			// 三种"当未命中处理"的情况，理由各不相同：
			//   res == nil  → 键不存在，正常的未命中
			//   !ok         → 类型不对（理论上不会发生）
			//   Unmarshal 失败 → 坏数据。当未命中处理，L3 会把正确值写回去，
			//                    相当于**顺手自愈**（和 GetDetail 里
			//                    "反序列化失败当未命中，锁+回填会覆盖它"同一条思路）
			str, ok := res.(string)
			if res == nil || !ok {
				missedL2 = append(missedL2, id)
				continue
			}
			var v video.Video
			if err := json.Unmarshal([]byte(str), &v); err != nil {
				missedL2 = append(missedL2, id)
				continue
			}

			videoMap[id] = &v
			// 命中 L2 就顺手回填 L1：下一次同进程的请求连 Redis 都不用去了。
			// 这一步是"L1 命中率"的来源 —— 没有它，L1 只有在刚回源过的那 5 秒
			// 内有内容，冷启动后的第一批请求永远打不到 L1。
			f.localcache.Set(cacheKeys[i], v, l1EntryTTL)
		}
	}

	if len(missedL2) == 0 {
		return buildOrderedResult(videoIDs, videoMap), nil
	}

	// ---------------------------------------------------------------
	// L3：MySQL —— 逐 id 并发 + singleflight + 异步回写
	// ---------------------------------------------------------------
	//
	// ---------- 为什么这里用 goroutine + singleflight，而不是 SETNX 锁 ----------
	//
	// 一个请求要还原**一整页** id（10~50 条）。用 SETNX 锁的话，要么：
	//   一次锁住整页 → 锁粒度太粗，10 个不同用户刷到部分重叠的页就会互等
	//   每 id 一把锁 → 一页 50 条就是 50 次 SETNX + 50 次 evalsha，
	//                   还没查到数据就先在 Redis 上花掉 100 次往返
	// 而 singleflight 是**进程内**的 map 锁，零网络开销。
	// 代价是它挡不住跨实例 —— 但这里可以接受：多查几次库的后果只是慢，
	// 不像详情页的击穿会造成陈旧的 likes_count 被写回（那边必须跨实例互斥）。
	//
	// 所以本项目的选择是：**详情用分布式锁，批量实体用 singleflight**。
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, id := range missedL2 {
		wg.Add(1)
		go func(videoID uint) {
			defer wg.Done()

			// sfKey 不是 Redis key！它只是进程内 singleflight 的合并键。
			// 这里借用了 rediscache.Key() 拼前缀，纯粹是为了让所有
			// "sf:" 开头的合并键长得一致、在日志里能一眼认出来 ——
			// 它永远不会被发给 Redis。（文档专门提了这一点，因为
			// `redis-cli KEYS sf:*` 查不到东西时很容易怀疑是 bug）
			sfKey := f.rediscache.Key("sf:entity:%d", videoID)

			v, err, _ := f.requestGroup.Do(sfKey, func() (interface{}, error) {
				// 注意这里用的仍是**第一个调用者的 ctx**。
				// 这是 singleflight 的经典坑：第一个请求超时/断开会让
				// 所有共享这次调用的请求一起失败（即使它们自己的 ctx 还好）。
				// 原项目如此，本项目也保留 —— 修它要给 ctx 做 WithoutCancel
				// 加独立超时，属于"知道有这个问题、但没量过收益"的改动，
				// 记在这里而不是默默改掉。
				list, err := f.repo.GetByIDs(ctx, []uint{videoID})
				if err != nil {
					return nil, err
				}
				if len(list) == 0 {
					// 视频不存在（已删）。返回 nil 而**不是**错误：
					// "查不到"是正常结果，不是故障 —— 上层靠 len() 判断，
					// 这里返回 err 会让整页请求 500。
					return nil, nil
				}
				got := list[0]

				// ---------- 异步回写 L2 ----------
				//
				// 开一条新 goroutine，用独立的 ctx（Background + 50ms），
				// 不接请求 ctx。两个原因：
				//   ① 回写耗时不算在请求头上（同步写的话等于给每个请求
				//      加一次 Redis 往返，白等）
				//   ② 客户端断开不该让回写半途而废 —— 数据已经查出来了，
				//      白白丢掉下次还要再查一遍
				// Set 失败只记日志：TTL 和 L3 都能兜住，不影响这次请求。
				if b, err := json.Marshal(*got); err == nil {
					key := f.rediscache.Key("video:entity:%d", got.ID)
					go func(k string, payload []byte) {
						setCtx, cancelSet := context.WithTimeout(context.Background(), entitySetTimeout)
						defer cancelSet()
						if err := f.rediscache.SetBytes(setCtx, k, payload, entityCacheTTL); err != nil {
							log.Printf("[feed] 回写 video:entity 失败 key=%s: %v", k, err)
						}
					}(key, b)
				}
				return got, nil
			})

			if err != nil || v == nil {
				return
			}

			// 存值不存指针：三个 goroutine 从 singleflight 拿到的是**同一个**
			// *video.Video（这是 singleflight 的语义，shared 结果被多方读取）。
			// 直接把它塞进 map 和 L1 会让多方共享一个可变结构体。
			// 拷一份，各自持有自己的副本 —— 这里一个结构体的拷贝远比
			// "将来某天有人在别处改了一个字段"的排查成本便宜。
			copied := *(v.(*video.Video))

			mu.Lock()
			videoMap[videoID] = &copied
			mu.Unlock()

			f.localcache.Set(f.rediscache.Key("video:entity:%d", copied.ID), copied, l1EntryTTL)
		}(id)
	}
	wg.Wait()

	// 已知缺口（记在这里，不在本阶段修）：
	// **查不到的 id 不写空值**，所以一个已删除视频的 id 只要还在某个
	// Redis 结构（ZSET/List）里，每次请求都会穿透 L1/L2 打到 MySQL。
	// 原项目同样如此。修法是在 L3 把 "NOT_FOUND" 用**更短的 TTL**（如 60s）
	// 也缓存起来 —— 不能和正常条目同 TTL，否则删除期间的误判会持续 1 小时。
	// 这属于文档里"缓存穿透"那一类，本阶段的清单没要求。
	return buildOrderedResult(videoIDs, videoMap), nil
}

// buildOrderedResult 把 map 里的结果按**输入 id 的顺序**重排。
//
// 这一步绝对不能省，而且省了不会有任何报错 —— 只会表现为两种很难查的症状：
//
//	① 顺序随机：L1 是按输入顺序命中的，但 L2 的 MGet 结果、L3 的 goroutine
//	   完成顺序**全都是乱的**。直接返回 map 的遍历结果，Go 的 map 遍历是
//	   随机化的，同一页视频每次请求顺序都不一样，前端表现是"一下拉顺序就变"。
//
//	② 游标分页错乱（更严重）：ListLatest 用**最后一条**的 create_time 当
//	   下一页游标。顺序错了，nextTime 拿到的就不是真正最老的那条 ——
//	   下一页会重复返回已经看过的视频，或者整段跳过。
//	   这类 bug 在功能测试里几乎测不出来（数据都在，只是顺序/边界错），
//	   只有翻页翻到底才会暴露。
//
// 查不到的 id **直接丢掉**，不填空洞 —— 和 hydrateInOrder 同一条纪律：
// 快照语义下视频被删了就少一条，填个空对象反而是脏数据。
func buildOrderedResult(videoIDs []uint, videoMap map[uint]*video.Video) []*video.Video {
	out := make([]*video.Video, 0, len(videoMap))
	for _, id := range videoIDs {
		if v, ok := videoMap[id]; ok {
			out = append(out, v)
		}
	}
	return out
}
