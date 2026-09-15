package account

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"myfeed/internal/auth"
	rediscache "myfeed/internal/middleware/redis"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

var (
	ErrNewUsernameRequired = errors.New("new_username is required")
	// 改进点：用户不存在和密码错误返回同一个错误（防"用户名枚举"），handler 映射 401
	ErrInvalidCredentials = errors.New("invalid username or password")
)

// token 缓存的两个 TTL。和原项目一致：access 24h（和 JWT 过期同量级），refresh 7d。
const (
	accessTokenCacheTTL  = 24 * time.Hour
	refreshTokenCacheTTL = 7 * 24 * time.Hour
)

// cacheOpTimeout 所有缓存操作的短超时。纪律：Redis 抖一下最多损失 50ms，
// 超时和故障都走 DB 兜底 —— 缓存加速，但绝不阻塞、不阻断主请求。
const cacheOpTimeout = 50 * time.Millisecond

// AccountService 业务逻辑层：管密码哈希和双 token 的签发决策。
// 不碰 SQL（repo 的事），不碰 gin（handler 的事）。
//
// cache 可以为 nil（启动时 Redis 连不上）：那样所有缓存操作静默跳过，
// 鉴权和 refresh 都走 DB 兜底路径，功能完整、只是慢 —— 这就是启动降级。
type AccountService struct {
	accountRepository *AccountRepository
	cache             *rediscache.Client
}

func NewAccountService(accountRepository *AccountRepository, cache *rediscache.Client) *AccountService {
	return &AccountService{accountRepository: accountRepository, cache: cache}
}

// CreateAccount 注册：把明文密码哈希后交给 repo 落库
func (as *AccountService) CreateAccount(ctx context.Context, account *Account) error {
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(account.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	account.Password = string(passwordHash)
	return as.accountRepository.CreateAccount(ctx, account)
}

// Login 登录：查人 → 验密码 → 签双 token → 覆盖存储（顶号下线）→ 写缓存三把。
//
// 三把缓存各自的用途（详见 07 文档的 key 表）：
//
//	account:<id>          当前 access token（24h）—— jwt 中间件的快路径就查这把
//	account:<id>:refresh  当前 refresh token（7d）—— 原项目无读者，留着对齐
//	refresh:<token>       refreshToken → accountID（7d）—— Refresh 快路径的反向索引，
//	                       它把"全表扫比对 refresh_token"变成一次点查
//
// **写缓存失败只记日志，不影响登录**：DB 才是真相源，缓存没写上顶多是
// 下一次鉴权走 DB 兜底（顺便自愈回填），绝不能因为 Redis 抖动就让用户登录失败。
func (as *AccountService) Login(ctx context.Context, username, password string) (string, string, error) {
	account, err := as.accountRepository.FindByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", ErrInvalidCredentials
		}
		return "", "", err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(password)); err != nil {
		return "", "", ErrInvalidCredentials
	}
	accessToken, err := auth.GenerateToken(account.ID, account.Username)
	if err != nil {
		return "", "", err
	}
	refreshToken, err := auth.GenerateRefreshToken(account.ID)
	if err != nil {
		return "", "", err
	}
	if err := as.accountRepository.Login(ctx, account.ID, accessToken, refreshToken); err != nil {
		return "", "", err
	}

	if as.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, cacheOpTimeout)
		defer cancel()

		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", account.ID), []byte(accessToken), accessTokenCacheTTL); err != nil {
			log.Printf("[account] 写 access token 缓存失败: %v", err)
		}
		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d:refresh", account.ID), []byte(refreshToken), refreshTokenCacheTTL); err != nil {
			log.Printf("[account] 写 refresh token 缓存失败: %v", err)
		}
		// 反向索引：refreshToken → accountID 的十进制串。
		// Refresh 快路径靠它把 O(N) 全表扫变成 O(1) 点查
		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("refresh:%s", refreshToken), []byte(strconv.FormatUint(uint64(account.ID), 10)), refreshTokenCacheTTL); err != nil {
			log.Printf("[account] 写 refresh 反向索引失败: %v", err)
		}
	}

	return accessToken, refreshToken, nil
}

// Logout 登出：先删缓存三把，再清 DB 双 token。能走到这说明中间件已验过身份。
//
// **删缓存是正确性问题不是优化**：jwt 的 check() 快路径只比对 Redis 里的 token。
// 如果登出不删 `account:<id>`，那旧 token 在 TTL（最长 24h）内仍然能通过缓存校验 ——
// 用户明明登出了，token 却还有效，这是实打实的越权。
//
// 顺序是"先删缓存再删 DB"，不是反过来。反过来会有一个窗口：
// DB 已清空但缓存还在 → 这个窗口内旧 token 仍能过①快路径。
// 先删缓存的话，最坏情况是缓存删了、DB 删除失败 → 下次鉴权落到 DB，
// 而 DB 里旧 token 还在 → 用户仍然有效（等于没登出，但下次重试就行，且不是越权）。
func (as *AccountService) Logout(ctx context.Context, accountID uint) error {
	// 先查出 account 拿到 RefreshToken —— `refresh:<token>` 这把缓存的 key 里
	// 含 token 本身，不查 DB 就不知道要删哪把（缓存里没有反向的反向索引）
	account, err := as.accountRepository.FindByID(ctx, accountID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // 已经不存在了，幂等返回成功
		}
		return err
	}
	if account.Token == "" {
		return nil // 已经登出过，幂等
	}

	if as.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, cacheOpTimeout)
		defer cancel()

		if err := as.cache.Del(cacheCtx, as.cache.Key("account:%d", account.ID)); err != nil {
			log.Printf("[account] 删 access token 缓存失败: %v", err)
		}
		if err := as.cache.Del(cacheCtx, as.cache.Key("account:%d:refresh", account.ID)); err != nil {
			log.Printf("[account] 删 refresh token 缓存失败: %v", err)
		}
		// 仅当 DB 里 RefreshToken 非空才删这把 —— 它从来没写过的话，
		// Del 一个不存在的 key 是 no-op，但 key 里会拼进空串，没意义
		if account.RefreshToken != "" {
			if err := as.cache.Del(cacheCtx, as.cache.Key("refresh:%s", account.RefreshToken)); err != nil {
				log.Printf("[account] 删 refresh 反向索引失败: %v", err)
			}
		}
	}

	return as.accountRepository.Logout(ctx, account.ID)
}

// ChangePassword 改密：验旧密码 → 哈希新密码 → 更新 → 双 token 全部作废。
//
// **必须调 as.Logout（带缓存清理）而不是 repo.Logout（只清 DB）**：
// 改密后旧 token 立即失效是安全要求，而 jwt 快路径只信缓存 ——
// 直接调 repo.Logout 会留下"DB 已清空、缓存还有旧 token"的状态，
// 旧 token 在 24h 内仍然有效。走 as.Logout 则三把缓存一起清掉。
func (as *AccountService) ChangePassword(ctx context.Context, username, oldPassword, newPassword string) error {
	account, err := as.accountRepository.FindByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrInvalidCredentials
		}
		return err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(oldPassword)); err != nil {
		return ErrInvalidCredentials
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := as.accountRepository.ChangePassword(ctx, account.ID, string(passwordHash)); err != nil {
		return err
	}
	// 密码改了 → 所有旧 token 立即作废（包括当前请求用的这个）
	return as.Logout(ctx, account.ID)
}

// Rename 改名：先签新 token（用新名字！），再和改名进同一事务，旧 token 立即失效。
//
// **事务成功后必须覆盖缓存**，这是本方法唯一的缓存操作，也是正确性问题：
// 改名 = 换新 token，DB 里 `accounts.token` 已经是新值，
// 而 Redis 里 `account:<id>` 还是旧 token。jwt 快路径比对的是缓存值，
// 不覆盖的话 —— **改名接口刚返回的新 token，下一个请求就 401**，用户被自己踢下线。
// 这种 bug 极难排查：日志里全是"token has been revoked"，但 DB 里 token 明明是对的。
func (as *AccountService) Rename(ctx context.Context, accountID uint, newUsername string) (string, error) {
	if newUsername == "" {
		return "", ErrNewUsernameRequired
	}
	token, err := auth.GenerateToken(accountID, newUsername)
	if err != nil {
		return "", err
	}
	if err := as.accountRepository.RenameWithToken(ctx, accountID, newUsername, token); err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return "", ErrUsernameTaken
		}
		return "", err
	}

	if as.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, cacheOpTimeout)
		defer cancel()

		if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", accountID), []byte(token), accessTokenCacheTTL); err != nil {
			// 写失败不返回错误：DB 已经改成功了，改名这件事成了。
			// 缓存没写上的后果是下一次鉴权走 DB 兜底（并自愈回填），慢一点而已。
			log.Printf("[account] 改名后覆盖 token 缓存失败: %v", err)
		}
	}

	return token, nil
}

// RefreshAccessToken 用 refreshToken 换新 accessToken 并覆盖旧 access token。
//
// 两条路径：
//
//	快路径：Get `refresh:<token>` 拿到 accountID → 点查这一个账号 → 签新 token
//	慢路径：FindAll 全表扫描逐条比对 refresh_token（快路径没走通时的兜底）
//
// 慢路径是 O(N)：账号表 10 万行就要扫 10 万行。快路径是 O(1) 点查。
// **缓存的价值在这里最直观** —— 它把一次全表扫描换成一次主键查询。
//
// 但快路径**仍然要查 DB 复核 refresh_token**，不能只信缓存值：
// 缓存可能脏（写了之后 DB 侧被别的路径改过）、可能错位（TTL 比 DB 状态活得久）。
// 原则和 jwt 一致：**缓存做索引，DB 做裁判**。
func (as *AccountService) RefreshAccessToken(ctx context.Context, refreshToken string) (string, uint, string, error) {
	if refreshToken == "" {
		return "", 0, "", errors.New("refresh token is empty")
	}

	if as.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, cacheOpTimeout)
		defer cancel()

		b, err := as.cache.GetBytes(cacheCtx, as.cache.Key("refresh:%s", refreshToken))
		if err == nil {
			id, parseErr := strconv.ParseUint(string(b), 10, 64)
			if parseErr == nil {
				account, findErr := as.accountRepository.FindByID(ctx, uint(id))
				// 复核：DB 里的 refresh_token 必须和请求带的一致（缓存只当索引用）
				if findErr == nil && account != nil && account.RefreshToken == refreshToken {
					newToken, genErr := auth.GenerateToken(account.ID, account.Username)
					if genErr != nil {
						return "", 0, "", genErr
					}
					if err := as.accountRepository.UpdateToken(ctx, account.ID, newToken); err != nil {
						return "", 0, "", err
					}
					// 回写 access token 缓存：新 token 立刻能过 jwt 快路径
					if err := as.cache.SetBytes(cacheCtx, as.cache.Key("account:%d", account.ID), []byte(newToken), accessTokenCacheTTL); err != nil {
						log.Printf("[account] refresh 后回写 token 缓存失败: %v", err)
					}
					return newToken, account.ID, account.Username, nil
				}
			}
			// 解析失败 / DB 复核不过 → 不报错，掉到慢路径再确认一次
			// （缓存脏是常态，慢路径才是最终裁判）
		}
	}

	// 慢路径：全表扫描比对（阶段1 的原始实现，现在降级为兜底）
	accounts, err := as.accountRepository.FindAll(ctx)
	if err != nil {
		return "", 0, "", err
	}
	for _, acc := range accounts {
		if acc.RefreshToken == refreshToken {
			newToken, err := auth.GenerateToken(acc.ID, acc.Username)
			if err != nil {
				return "", 0, "", err
			}
			if err := as.accountRepository.UpdateToken(ctx, acc.ID, newToken); err != nil {
				return "", 0, "", err
			}
			return newToken, acc.ID, acc.Username, nil
		}
	}
	return "", 0, "", errors.New("invalid refresh token")
}

// FindByID / FindByUsername 查询直通 repo
func (as *AccountService) FindByID(ctx context.Context, id uint) (*Account, error) {
	return as.accountRepository.FindByID(ctx, id)
}

func (as *AccountService) FindByUsername(ctx context.Context, username string) (*Account, error) {
	return as.accountRepository.FindByUsername(ctx, username)
}

// UpdateAvatar 头像 URL 落库（文件保存本身在 handler）
func (as *AccountService) UpdateAvatar(ctx context.Context, accountID uint, avatarURL string) error {
	return as.accountRepository.UpdateAvatar(ctx, accountID, avatarURL)
}

// UpdateProfile 按需更新 bio/avatar_url，两字段都空则拒绝
func (as *AccountService) UpdateProfile(ctx context.Context, accountID uint, req *UpdateProfileRequest) error {
	updates := map[string]interface{}{}
	if req.Bio != "" {
		updates["bio"] = strings.TrimSpace(req.Bio)
	}
	if req.AvatarURL != "" {
		updates["avatar_url"] = strings.TrimSpace(req.AvatarURL)
	}
	if len(updates) == 0 {
		return errors.New("nothing to update")
	}
	return as.accountRepository.UpdateFields(ctx, accountID, updates)
}
