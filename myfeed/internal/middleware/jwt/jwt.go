package jwt

import (
	"errors"
	"net/http"
	"strings"

	"myfeed/internal/account"
	"myfeed/internal/auth"

	"github.com/gin-gonic/gin"
)

// JWTAuth 强鉴权：无 header / 格式错 / 验签失败 / 已撤销 → 401
// 阶段7回填：签名加 cache *rediscache.Client 参数，check() 里插入 Redis 优先逻辑
func JWTAuth(accountRepo *account.AccountRepository) gin.HandlerFunc {
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

		check(c, claims, tokenString, accountRepo)
	}
}

// SoftJWTAuth 软鉴权：没带 header → 匿名放行；带了但不对 → 401（带了就必须是对的）
// 阶段6 的 Feed 用：匿名也能刷 Feed，但登录用户要带 token（is_liked/关注流需要身份）
// 阶段7回填：同 JWTAuth
func SoftJWTAuth(accountRepo *account.AccountRepository) gin.HandlerFunc {
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

		check(c, claims, tokenString, accountRepo)
	}
}

// check 单会话撤销校验：库里存的 token 必须和请求带来的一致。
// 三种失败都算"已撤销"：查库失败 / 库里为空（登出、改密）/ 不相等（顶号、改名）
// 阶段7回填：先查 Redis（50ms 超时），未命中回落 DB 并回写缓存
func check(c *gin.Context, claims *auth.Claims, tokenString string, accountRepo *account.AccountRepository) {
	accountInfo, err := accountRepo.FindByID(c.Request.Context(), claims.AccountID)
	if err != nil || accountInfo.Token == "" || accountInfo.Token != tokenString {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token has been revoked"})
		return
	}

	// 把身份挂到请求上下文上，后续 handler 通过 GetAccountID/GetUsername 取
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
