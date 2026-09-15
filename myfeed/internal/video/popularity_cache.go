package video

import (
	"context"
	"strconv"
	"time"

	rediscache "myfeed/internal/middleware/redis"
)

// 热榜写路径（阶段8）。点赞/取消/评论在 **MySQL 提交之后**调这里，做两件事：
//
//	① 失效该视频的两个详情缓存（它自己的 popularity 变了）
//	② 往"当前分钟"的桶里 +1/-1
//
// 阶段9 会把调用方从 service 换成 PopularityWorker —— 本文件一行都不用改，
// 只是 `ctx` 从"HTTP 请求的 ctx"变成"MQ consumer 的 ctx"。
//
// ---------- 为什么整套动作都是"尽力而为" ----------
//
// 热度是**旁路数据**：它不影响点赞能不能成功、评论能不能发出。
// 所以这里所有 Redis 操作都忽略错误、都有超时、失败不做任何补偿 ——
// 热榜少一个数远好过用户点不动赞。
//
// 这和阶段7 的 fail-open 限流是同一种取向，但注意**失效方向相反**：
//
//	限流      Redis 挂了 → 放行（宁可漏限，不可误拦）
//	热度      Redis 挂了 → 不写（宁可少算，不可拖垮主流程）
//	登录态    Redis 挂了 → 查库（宁可慢，不可误判未登录）
//
// 同一套 Redis、三种失败取向，判据都是"这个东西坏了，哪种错更不可接受"。
const (
	// popularityBucketTTL 是分钟桶的存活时间：60 分钟窗口的 2 倍冗余。
	//
	// 冗余是为了覆盖两种"窗口还没滚完但桶已经该在"的情况：
	//   - Worker 重启期间漏写的分钟（阶段9）
	//   - 客户端拿一个 1 小时前的 as_of 回来续翻（难点1 末尾的过期 as_of）
	//
	// **每次写都重新续到 2h**（滚动过期），所以一个桶的真实寿命是
	// "最后一次互动 + 2h"——活跃的桶一直活着，冷掉的桶自然消失，
	// 不需要任何定时器。这就是分钟桶"天然时间衰减"的实现方式。
	popularityBucketTTL = 2 * time.Hour

	// popularityCacheOpTimeout 写路径的超时。比读路径的 80ms 短 ——
	// 写是在**用户的点赞请求里同步做的**，它有资格占用主流程的时间更少。
	popularityCacheOpTimeout = 50 * time.Millisecond

	// PopularityBucketLayout 分钟桶 key 的时间后缀格式。
	//
	// 参考时间 "200601021504" 是 Go 的时间布局魔数（2006-01-02 15:04:05 的
	// 年月日时分秒），少一个字符、错一位，key 就全乱 ——
	// 而且**读路径和写路径会各自算出一套不同的 key，谁都不报错**，
	// 现象是"热榜永远空"，极难定位。
	//
	// 所以它被**导出**，且全项目只有这一份定义：读路径（feed 包）不自己
	// 拼字符串，而是调下面的 PopularityBucketKey —— 格式错位的可能性被
	// 从"两处要写对"降成"一处要写对"。
	//
	// 时区：格式里**没有**时区信息，所以时间基准必须统一 —— 三个构造函数
	// 里都固定先 `.UTC().Truncate(time.Minute)`，调用方无法绕过。
	PopularityBucketLayout = "200601021504"

	// PopularityWindowMinutes 是滑窗宽度：60 个分钟桶 = 最近 1 小时。
	//
	// 它同时是**读路径一次 ZUNIONSTORE 的 K**。ZUNIONSTORE 是 O(N*K)，
	// 这个数直接决定聚合成本 —— 所以它是个有性能含义的常量，不是随便定的。
	// （文档难点3 的警告：开成 24h = 1440 个桶会阻塞 Redis 主线程几十上百 ms。）
	PopularityWindowMinutes = 60

	// PopularitySnapshotTTL 快照的存活时间：2 分钟。
	//
	// 只要 > 一次典型翻页会话的时长就够：翻页期间 as_of 不变，同一分钟内的
	// 所有请求复用同一份快照（榜单冻结）；2 分钟一到，快照消失，
	// 下一个请求按新的 as_of 重算 —— 榜单自然"向前滚一格"。
	//
	// 为什么不是更长：快照是内存里最大的一份数据（member 集合 = 60 分钟内
	// 所有被互动过的视频）。TTL 直接决定同时存活的快照份数。
	PopularitySnapshotTTL = 2 * time.Minute
)

// ---------- 桶 key 的三个构造函数 ----------
//
// 读写两侧**只能**通过它们拿 key，不许自己 Format。这治的是难点1 的第一条：
// "两处必须一致，否则读写各自算出一套 key 且谁都不报错"。
//
// 三个函数里都做了 `.UTC().Truncate(time.Minute)`，顺序固定"先 UTC 再截断" ——
// 这个顺序不能反，也不能由调用方决定。截断是按绝对时间戳向下取整的，
// 先截断再转时区、和先转时区再截断，在整点以外的分钟上结果一样、
// 但**字符串**会差一个时区偏移，于是 key 完全不同。

// PopularityBucketKey 某个时刻所在的分钟桶 key。
// 写路径传 time.Now()，读路径传 asOf.Add(-i*time.Minute)。
func PopularityBucketKey(cache *rediscache.Client, t time.Time) string {
	return cache.Key("hot:video:1m:%s", t.UTC().Truncate(time.Minute).Format(PopularityBucketLayout))
}

// PopularitySnapshotKey 某个 as_of 对应的快照 key。
// 快照按分钟命名，所以"同一分钟的所有请求"天然指向同一个 key —— 这是
// 快照能被复用的前提，不需要额外的锁或标记。
func PopularitySnapshotKey(cache *rediscache.Client, asOf time.Time) string {
	return cache.Key("hot:video:merge:1m:%s", asOf.UTC().Truncate(time.Minute).Format(PopularityBucketLayout))
}

// PopularityWindowKeys 返回 asOf 及其往前共 60 个桶的 key，**从新到旧**排列。
//
// i=0 是 asOf 本分钟 —— 所以"正在进行的这一分钟"也在窗口里，首屏因此有
// 准实时性（刚点的赞马上能上榜）。翻页时 asOf 已经是过去的分钟，
// 60 个桶全部冻结，快照过期重算的结果也完全一致（难点2 的"重算确定性"）。
//
// 顺序（新→旧）只影响可读性，不影响结果：ZUNIONSTORE 是集合求并，
// 与参数顺序无关。**但 ZREVRANGE 的顺序完全由 score 决定**，
// 和这里的排列无关 —— 别把两者混了。
func PopularityWindowKeys(cache *rediscache.Client, asOf time.Time) []string {
	keys := make([]string, 0, PopularityWindowMinutes)
	for i := 0; i < PopularityWindowMinutes; i++ {
		keys = append(keys, PopularityBucketKey(cache, asOf.Add(-time.Duration(i)*time.Minute)))
	}
	return keys
}

// UpdatePopularityCache 把一个热度增量记进"当前分钟"的桶，并失效相关缓存。
//
// 参数 change 是**增量**（+1/-1），不是目标值 —— 和 ChangePopularityTx 同一个语义。
// 因为 ZINCRBY 本身就是原子累加，传增量才能让并发请求各自生效。
//
// 幂等性：**不幂等**。重复调用会重复累加，所以它只能在"事件确定发生了一次"的
// 地方调用（事务提交后）。阶段9 换成 MQ 之后，靠 event_id 去重来保证这点。
func UpdatePopularityCache(ctx context.Context, cache *rediscache.Client, id uint, change int64) {
	// 守卫三连：
	//   cache == nil  启动时 Redis 就没连上（cmd/main.go Ping 失败）→ 整条链路跳过
	//   id == 0       参数异常。不 guard 的话会往桶里写一个 member="0"，
	//                 而读路径 ParseUint 后看到 0 会丢弃它 —— 白写一个脏 member
	//   change == 0   没有增量。**必须挡**：ZINCRBY 0 会在桶里创建一个
	//                 score=0 的 member，让这个视频白白出现在热榜末尾
	if cache == nil || id == 0 || change == 0 {
		return
	}

	// ---------- 关于 context.WithoutCancel ----------
	//
	// 这里所有 Redis 操作都挂在 WithoutCancel(ctx) 上，而不是直接用 ctx。
	//
	// 原因：调用点在 Like/Unlike/Comment 里，而**客户端可能在点赞请求返回到一半时
	// 断开连接**，此时 ctx 被取消。如果用 ctx，Redis 操作会立刻失败 ——
	// 而 MySQL 那边的 popularity+1 已经提交了，两边就此分叉，且没有任何日志。
	//
	// WithoutCancel 保留 ctx 里的值（如链路追踪的 traceID），只丢掉取消信号 ——
	// 正好是"这个写必须做完，但它属于哪个请求我还是想知道"的语义。
	// Go 1.21+ 才有；本项目的 go.mod 是 1.26。
	//
	// 超时则**必须有**：Redis 半死不活（TCP 连得上但不回包）时，
	// ctx 不取消不代表它不会永远挂着。50ms 是兜底。

	// ---------- ① 失效两个详情缓存 ----------
	//
	// 两个 key 都必须在场，漏掉任何一个都会让用户看到旧数据：
	//
	//   video:detail:id=<id>   getDetail 的响应缓存（阶段7）
	//   video:entity:<id>      feed 的 GetVideoByIDs L2 实体缓存（阶段7）
	//
	// 具体是哪两个 key、为什么它们归属不同的包、以及 WithoutCancel 和超时的
	// 理由，全部收在 InvalidateVideoCaches 里（阶段 C 转码 worker 也要用同一份）。
	// 这里**不内联那几个 key**：转码 worker 改的是同两个字段，
	// 各写一份的话，将来加第三种副本只会加在其中一个地方。
	InvalidateVideoCaches(ctx, cache, id)

	// ---------- ② 往分钟桶里累加 ----------
	//
	// 桶的粒度是"分钟"，不是"视频"也不是"全局"：
	//   - 按视频分桶 → 写热点集中在一个 key 上
	//   - 全局单 key  → 同上，且历史热度永远累计，无法表达"近期热门"
	//   - 按分钟分桶  → 写冲突被 60 个桶摊开，老桶滚出窗口即消失
	//
	// 桶 key 走 PopularityBucketKey（和读路径同一个函数）—— 见那个函数上
	// 关于"先 UTC 再 Truncate"的说明。
	windowKey := PopularityBucketKey(cache, time.Now())

	// member 用**裸十进制字符串**，不加前缀、不用 JSON。
	//
	// 这不是审美问题，是两个硬约束：
	//   ① 读路径要用 strconv.ParseUint 还原成 ID，加了前缀就得多一次字符串裁剪
	//   ② 同分时 ZSET 按 member 的**字典序**定序（实测：score 全为 5 的
	//      7/10/9/100 → ZREVRANGE 输出 9 7 100 10）。member 编码一旦变化，
	//      同分视频的相对顺序就变了 → 翻页时名次错位。编码固定 = 顺序确定。
	//
	// 注意这个字典序**不是数值序**（100 排在 10 前面）。排行榜只需要"确定"，
	// 不需要"数值有序"—— 真正的排序键是 score。
	member := strconv.FormatUint(uint64(id), 10)

	opCtx, cancelOp := context.WithTimeout(context.WithoutCancel(ctx), popularityCacheOpTimeout)
	defer cancelOp()

	// 错误全部忽略：见文件头"尽力而为"。但要知道失败**不会被补偿** ——
	// 阶段8 的桶没有第二份数据源，写丢了就是丢了（MySQL 的 popularity 是累计值，
	// 语义不同，不能用来重建"近 60 分钟"。阶段9 的 MQ 重试才是补偿手段）。
	_ = cache.ZincrBy(opCtx, windowKey, member, float64(change))

	// EXPIRE 单独一步，且**必须在场**。
	//
	// 实测（redis-cli）：ZINCRBY 隐式建 key 时**不带 TTL**，此时 `TTL` 返回 -1。
	// 也就是说 ZINCRBY 和 EXPIRE 之间如果进程被杀，这个桶会**永生** ——
	// 每分钟泄漏一个 key，而且永远不会自己消失。
	//
	// 每次写都续期到 2h，"滚动过期"：活跃的桶被不断续命，冷掉的桶自己到期。
	// （彻底方案是把两条命令塞进一个 Lua 脚本原子化，属于进阶改进，原项目也没做。）
	_ = cache.Expire(opCtx, windowKey, popularityBucketTTL)
}
