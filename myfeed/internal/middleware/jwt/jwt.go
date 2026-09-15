package jwt

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"myfeed/internal/account"
	"myfeed/internal/auth"
	rediscache "myfeed/internal/middleware/redis"

	"github.com/gin-gonic/gin"
)

// JWTAuth 强鉴权：无 header / 格式错 / 验签失败 / 已撤销 → 401
func JWTAuth(accountRepo *account.AccountRepository, cache *rediscache.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header"})
			return
		}

		tokenString := parts[1]

		claims, err := auth.ParseToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		check(c, claims, tokenString, accountRepo, cache)
	}
}

// QueryTokenAuth 是**给 SSE 专用**的强鉴权：token 可以先从 `?token=` 取，
// 取不到再回落到 `Authorization: Bearer`。除此之外和 JWTAuth 完全一样。
//
// ---------- 为什么必须有这个后门 ----------
//
// 浏览器的 `EventSource` API **发不了自定义 header** —— 它的构造函数
// 只接受一个 URL，没有第二个参数可以塞 Authorization。这是 W3C 规范
// 层面的限制，不是浏览器偷懒，所以前端没有任何绕法：
//
//	const es = new EventSource("/notification/stream?token=" + jwt)  // 只能这样
//
// 而"把 token 放 URL 里"是有代价的，必须说清楚：
//
//	① token 会进服务器访问日志、进浏览器历史、进 Referer
//	   —— 本项目是本地学习环境，接受；生产环境应该改成一次性 ticket
//	      （先 POST 换一个短时效的 stream-ticket，再用它连 SSE）
//	② 前端**不能**用 fetch/axios 自己实现 SSE 来绕过它
//	   —— 那样能带 header，但就要自己解析 `data:` 协议和重连，
//	      等于重写一遍 EventSource，得不偿失
//
// ---------- 为什么是"先 query 再 header"这个顺序 ----------
//
// 反过来（先 header）也能工作，但 EventSource 场景下 query 一定存在、
// header 一定不存在，先查 header 等于每次白查一次。顺序按**实际命中率**排。
//
// ---------- 为什么它放在 jwt 包而不是 SSEHub 里 ----------
//
// 文档把它写成 hub 的方法（`hub.SSERequireAuth()`）。这里**刻意偏离**：
// 它和 JWTAuth 共享的 `check` 是**安全敏感**的（token 撤销检查、
// 自愈回填），里面每一行都是踩过坑的。放两份实现的后果是：
// 哪天修了撤销逻辑的漏洞，只改了 JWTAuth 那一份，SSE 这条路**悄悄**
// 继续放行已撤销的 token —— 而这正是最不该出错的地方。
//
// **安全逻辑的重复比业务逻辑的重复危险得多**，所以宁可让它离 check 近一点。
func QueryTokenAuth(accountRepo *account.AccountRepository, cache *rediscache.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString := c.Query("token")

		if tokenString == "" {
			authHeader := c.GetHeader("Authorization")
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				tokenString = parts[1]
			}
		}

		if tokenString == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token (use ?token= or Authorization header)"})
			return
		}

		claims, err := auth.ParseToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		// ← 和 JWTAuth 走的是**同一个** check（撤销检查在这里面）
		check(c, claims, tokenString, accountRepo, cache)
	}
}

// SoftJWTAuth 软鉴权：没带 header → 匿名放行；带了但不对 → 401（带了就必须是对的）
// 阶段6 的 Feed 用：匿名也能刷 Feed，但登录用户要带 token（is_liked/关注流需要身份）
func SoftJWTAuth(accountRepo *account.AccountRepository, cache *rediscache.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Next() // 匿名放行（fail-open）
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header"})
			return
		}

		tokenString := parts[1]

		claims, err := auth.ParseToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		check(c, claims, tokenString, accountRepo, cache)
	}
}

// check 单会话撤销校验 + 自愈缓存。三步走：
//
//	① Redis 命中且 token 一致        → 放行（最快路径，0 条 SQL）
//	② Redis 命中但 token 不一致      → 401（已撤销：登出/顶号/改名后缓存已更新）
//	③ Redis 未命中/故障/cache==nil   → 查 DB 兜底；通过后回填缓存（自愈）
//
// 核心哲学：**缓存加速但不做裁判** —— 裁判永远是 DB 里的 token 列。
// 鉴权是 fail-closed 到 DB：宁可慢（多一条 SQL）不可错（绝不让非法 token 过）。
//
// 自愈（self-healing）：第③步 DB 校验通过后顺手 SetBytes 回填，
// 所以 Redis 重启后不需要任何预热任务，流量自己会把缓存重新热起来。
func check(c *gin.Context, claims *auth.Claims, tokenString string, accountRepo *account.AccountRepository, cache *rediscache.Client) {
	// Key() 有 nil 接收器保护，cache==nil 时返回不带前缀的 key，这里无需判空就能调
	key := cache.Key("account:%d", claims.AccountID)

	// ① / ② 先查 Redis（50ms 超时，绝不拖垮主请求）
	if cache != nil {
		cacheCtx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
		defer cancel()

		b, err := cache.GetBytes(cacheCtx, key)
		if err == nil {
			// 命中。Redis 里存的就是当前合法 token，不一致 = 已撤销
			if string(b) != tokenString {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token has been revoked"})
				return
			}
			c.Set("accountID", claims.AccountID)
			c.Set("username", claims.Username)
			c.Next()
			return
		}
		// err != nil（未命中 redis.Nil / 超时 / 故障）→ 一律往下走 DB 兜底
	}

	// ③ DB 兜底。三种失败都算"已撤销"：查库失败 / 库里为空（登出、改密）/ 不相等（顶号、改名）
	accountInfo, err := accountRepo.FindByID(c.Request.Context(), claims.AccountID)
	if err != nil || accountInfo.Token == "" || accountInfo.Token != tokenString {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token has been revoked"})
		return
	}

	// 自愈回填：DB 校验通过了，把 token 写回 Redis（下次就走①快路径）
	if cache != nil {
		cacheCtx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
		defer cancel()

		if err := cache.SetBytes(cacheCtx, key, []byte(tokenString), 24*time.Hour); err != nil {
			log.Printf("[jwt] 回填 token 缓存失败（不影响本次鉴权）: %v", err)
		}
	}

	c.Set("accountID", claims.AccountID)
	c.Set("username", claims.Username)
	c.Next()
}

// GetAccountID 取出中间件塞进来的身份（断言 uint，GORM 主键类型）
func GetAccountID(c *gin.Context) (uint, error) {
	uidValue, exists := c.Get("accountID")
	if !exists {
		return 0, errors.New("accountID not found")
	}

	accountID, ok := uidValue.(uint)
	if !ok {
		return 0, errors.New("accountID has invalid type")
	}

	return accountID, nil
}

// GetUsername 取出中间件塞进来的用户名
func GetUsername(c *gin.Context) (string, error) {
	val, exists := c.Get("username")
	if !exists {
		return "", errors.New("username not found")
	}

	username, ok := val.(string)
	if !ok {
		return "", errors.New("username has invalid type")
	}

	return username, nil
}
