package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 缓存 secret，避免每个请求都读一次环境变量
var cachedSecret []byte

func jwtSecret() []byte {
	if cachedSecret != nil {
		return cachedSecret
	}
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "myfeed-dev-secret-change-me" // 开发期兜底（文档 Q3）
	}
	cachedSecret = []byte(secret)
	return cachedSecret
}

// Claims 就是 JWT 的"载荷"：我们自定义的身份字段 + 标准字段
type Claims struct {
	AccountID uint   `json:"account_id"`
	Username  string `json:"username"`
	jwt.RegisteredClaims
}

func GenerateToken(accountID uint, username string) (string, error) {
	now := time.Now()

	claims := Claims{
		AccountID: accountID,
		Username:  username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour)), // 过期时间
			IssuedAt:  jwt.NewNumericDate(now),                     // 签发时间
			NotBefore: jwt.NewNumericDate(now),                     // 生效时间
		},
	}

	// 用 HS256 算法 + secret 签名，得到最终的 token 字符串
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret())
}

func GenerateRefreshToken(accountID uint) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ParseToken 解析并验证 token：中间件每个请求调用
func ParseToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(
		tokenString,
		&Claims{},
		func(token *jwt.Token) (interface{}, error) {
			// 只认 HS256，防"算法混淆攻击"（讲解见下）
			if token.Method == nil || token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, errors.New("unexpected signing method")
			}
			return jwtSecret(), nil
		},
	)
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, jwt.ErrTokenInvalidClaims
	}
	return claims, nil
}
