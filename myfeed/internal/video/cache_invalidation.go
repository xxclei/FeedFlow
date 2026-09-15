package video

import (
	"context"

	rediscache "myfeed/internal/middleware/redis"
)

// 本文件只有一件事：**删掉所有存了一份 Video 副本的缓存 key**。
//
// ---------- 为什么它值得单独一个文件 ----------
//
// 缓存是散落的 —— 同一个 Video struct 被两个包以两种 key 各存了一份：
//
//	v1:video:detail:id=<id>   video 包的 getDetail 响应缓存（阶段7，TTL 5min）
//	v1:video:entity:<id>      feed 包的 GetVideoByIDs L2 实体缓存（阶段7，TTL 1h）
//
// 所以"改了 videos 表的某个字段"这句话，**在这份代码里不是一个局部改动** ——
// 它有两处需要通知，而这两处分别在两个包里。写字段的人（阶段 B 的门禁、
// 阶段 C 的转码 worker）不会自然地想到要去 feed 包里删一个 key。
//
// 阶段 C 就真的漏了一次：转码 worker 的 MarkTranscode 只写了 MySQL，
// 谁也没删这两个 key。后果是**转码成功但播放端永远拿不到 hls_url**：
//
//	发布 → 视频行是 pending / hls_url=''  → 上传者立刻打开自己的详情页
//	     → 这份 pending 被回填进缓存 → 转码完成，DB 变成 ready
//	     → 但 detail 缓存还是 pending，**5 分钟内所有人都看不到 hls_url**
//	     → 前端走直传 mp4 那条路，HLS 产物一次都不会被请求
//
// 它不报错、不崩、日志干净，只是"转码白做了"。这正是本项目最忌讳的失败形状，
// 也正是把这件事从"记得删"变成"一个函数"的理由 ——
// 调用方只需要知道"我改了 Video"，不需要知道有几个副本存在哪里。
//
// ---------- 为什么它接受 ctx 却不用它 ----------
//
// 见下面 WithoutCancel 那段。签名留 ctx 是为了**让调用方能传**（调用点是 HTTP
// 请求或 MQ 投递，各带各的 traceID），但取消信号一定要丢掉。
func InvalidateVideoCaches(ctx context.Context, cache *rediscache.Client, id uint) {
	if cache == nil || id == 0 {
		return
	}

	// context.WithoutCancel：**这个删除必须做完，哪怕请求已经结束了**。
	//
	// 同 UpdatePopularityCache 里那段（见那个文件的详细推导）：调用点的
	// ctx 来自 HTTP 请求或 MQ 投递，客户端完全可能在上一步落库之后、
	// 这一步删缓存之前断开。用 ctx 的话删除会立刻失败 ——
	// 而脏缓存留下的后果（用户看到旧数据）是**已经落库的那半**造成的，
	// 不能因为客户端走了就把它留在缓存里。
	//
	// 超时仍然必须有：Redis 半死不活（TCP 连得上但不回包）时，
	// "不取消"不代表"不会永远挂着"。50ms 是兜底。
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), popularityCacheOpTimeout)
	defer cancel()

	// DelMany（pipeline）而不是两次 Del：省一次往返。
	// 失败全忽略：TTL 是最终保底（detail 5min、entity 1h），最坏脏这么久 ——
	// 而这正是给缓存设 TTL 的意义，失效只是让"最坏"通常不发生。
	//
	// ⚠ 新增第三种副本时**必须加到这里**。漏加的表现和上面那次一模一样：
	// 没有错误、没有日志，只有一个字段永远停在旧值。
	_ = cache.DelMany(opCtx, []string{
		cache.Key("video:detail:id=%d", id),
		cache.Key("video:entity:%d", id),
	})
}
