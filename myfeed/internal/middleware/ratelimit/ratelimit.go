// Package ratelimit 提供基于 Redis 固定窗口计数的限流中间件。
//
// 它在阶段7 里的角色和缓存**相反**：缓存是"让快的更快"，限流是"让坏的慢下来"。
// 两者共用同一个 Redis client，但对 Redis 故障的取向完全相反 ——
// 详见 Limit 的注释。
package ratelimit

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	jwt "myfeed/internal/middleware/jwt"
	rediscache "myfeed/internal/middleware/redis"

	"github.com/gin-gonic/gin"
)

// KeyFunc 从请求里取出"限流主体"—— 也就是"现在是在限谁"。
//
// 第二个返回值是**取不到主体**的信号：匿名用户、或者主体压根不存在。
// 取不到一律放行，绝不因为"不知道该限谁"就把请求挡在门外。
type KeyFunc func(*gin.Context) (string, bool)

// Limit 固定窗口限流：每个窗口内最多放行 maxRequests 次。
//
// 计数器从 1 开始，第 maxRequests+1 次开始返回 429。窗口由上界那次 INCR
// 的 PEXPIRE 决定（见 redis.IncrementWithExpire），第一个请求负责种下过期时间。
//
// **四个提前放行的口子，每一个都是刻意的**：
//
//  1. 参数不合法（cache/keyFunc 为空、上限或窗口 <= 0）→ 放行。
//     中间件挂错了、配置漏了，那是部署问题，不该表现为"用户被拦住"。
//  2. 主体取不到（匿名）→ 放行。没有主体就没有"限谁"这件事。
//  3. INCR 出错（Redis 挂了/超时）→ 放行。
//  4. （顺带）cache 非 nil 但内部 rdb 为 nil → IncrementWithExpire 返回 (0, nil)
//     → count=0 不超限 → 放行。同一个口子的另一种走法。
//
// 这就是 **fail-open**，和鉴权的 fail-closed 方向相反：
//
//	鉴权挂了 → 落到 DB 继续校验（宁可多花时间也要把人认出来）
//	限流挂了 → 直接放行（宁可少一层保护也不能把正常用户挡在外面）
//
// 同一个 Redis，两种相反的故障取向。判断依据是同一个问题：
// **这条链路出问题时，哪一种错更不能接受？** 认不出人 = 数据错乱；
// 少限一次流 = 多几个请求。前者严重得多。
//
// 已知的局限（固定窗口的固有缺陷，不是实现 bug）：
// 窗口边界会有突刺 —— 10次/分 的配置下，10:59.9 打 10 次 + 11:00.0 打 10 次
// = 200 毫秒内放行 20 次。真要防这个得换滑动窗口或令牌桶，
// 但那是"更准"，不是"更对"，5次/时 的注册限流用固定窗口完全够。
func Limit(
	cache *rediscache.Client,
	keyPrefix string,
	maxRequests int64,
	window time.Duration,
	keyFunc KeyFunc,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cache == nil || keyFunc == nil || maxRequests <= 0 || window <= 0 {
			c.Next()
			return
		}

		subject, ok := keyFunc(c)
		if !ok {
			c.Next()
			return
		}

		key := cache.Key("ratelimit:%s:%s", normalizePrefix(keyPrefix), subject)
		// 用请求自身的 ctx：客户端断开时这次计数取消掉是合理的 ——
		// 一个没人要的请求没占住服务端资源，不该消耗用户额度。
		count, err := cache.IncrementWithExpire(c.Request.Context(), key, window)
		if err != nil {
			// fail-open。但**必须留日志** —— 否则"Redis 挂了 → 限流静默失效"
			// 这个状态在排查时是完全隐形的：现象是"限流没生效"，
			// 而没有日志的话，你会去怀疑中间件没挂上、key 拼错了、主体取不到，
			// 排查方向全错。
			log.Printf("[ratelimit] INCR 失败，放行 key=%s: %v", key, err)
			c.Next()
			return
		}

		if count > maxRequests {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many requests",
			})
			return
		}

		c.Next()
	}
}

// normalizePrefix 兜住空/空白前缀，避免出现 "v1:ratelimit::1" 这种
// 两个不同配置撞进同一个 key 空间的情况。
func normalizePrefix(keyPrefix string) string {
	keyPrefix = strings.TrimSpace(keyPrefix)
	if keyPrefix == "" {
		return "default"
	}
	return keyPrefix
}

// KeyByIP 按来源 IP 限流。用在**还没有账号**的接口上：注册、登录。
//
// 讽刺的是，这两个接口正是"攻击者可以无限造账号"的地方 ——
// 按账号限流在这里没有意义（他会换个账号），只能按 IP。
//
// c.ClientIP() 在这里拿到的是**直连 IP**，不是 X-Forwarded-For：
// router.go 里 `SetTrustedProxies(nil)` 关掉了代理信任，gin 只认 RemoteAddr。
// 好处是伪造不了（否则攻击者每个请求换一个假 XFF，限流等于没做）。
//
// **代价是阶段10 的坑**：一旦前面架上 nginx，所有请求的 RemoteAddr
// 都会变成 nginx 的地址，`5次/时` 会退化成"全站 5次/时"的注册封杀。
// 到那时必须把 SetTrustedProxies 改成信任内网代理网段，再从这个
// 可信来源的 XFF 里取真实 IP。现在这样写是**当下正确**（没有代理），
// 不是永远正确。
func KeyByIP(c *gin.Context) (string, bool) {
	ip := strings.TrimSpace(c.ClientIP())
	if ip == "" {
		return "", false
	}
	return ip, true
}

// KeyByAccount 按账号限流。用在已登录的写接口上：点赞、评论、关注。
//
// 依赖 c.Get("accountID") —— 那个值是 JWTAuth 写进去的。
// 所以**带账号限流的路由必须挂在 JWTAuth 之后**：
//
//	group.Use(jwt.JWTAuth(...))               // 先跑
//	group.POST("/like", likeLimiter, handler) // 后跑
//
// gin 里 group 级中间件先于路由级中间件执行，顺序天然正确。
// 但要是谁把 limiter 挂到 group.Use 上、或者挂到 JWTAuth 之前的组里，
// accountID 永远取不到 → 每请求都走"取不到主体 → 放行" → **限流静默失效**。
// 静默是关键：它不报错、不 panic、测试也测不出来，
// 只有压测时看到"打了 1000 次都没 429"才会发现。
func KeyByAccount(c *gin.Context) (string, bool) {
	accountID, err := jwt.GetAccountID(c)
	if err != nil || accountID == 0 {
		return "", false
	}
	return strconv.FormatUint(uint64(accountID), 10), true
}
