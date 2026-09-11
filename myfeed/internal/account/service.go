package account

import (
	"context"
	"errors"
	"strings"

	"myfeed/internal/auth"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

var (
	ErrNewUsernameRequired = errors.New("new_username is required")
	// 改进点：用户不存在和密码错误返回同一个错误（防"用户名枚举"），handler 映射 401
	ErrInvalidCredentials = errors.New("invalid username or password")
)

// AccountService 业务逻辑层：管密码哈希和双 token 的签发决策。
// 不碰 SQL（repo 的事），不碰 gin（handler 的事）。
// cache 参数阶段7回填。
type AccountService struct {
	accountRepository *AccountRepository
}

func NewAccountService(accountRepository *AccountRepository) *AccountService {
	return &AccountService{accountRepository: accountRepository}
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

// Login 登录：查人 → 验密码 → 签双 token → 覆盖存储（顶号下线）
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
	return accessToken, refreshToken, nil
}

// Logout 登出：双 token 全清。能走到这说明中间件已验过身份
func (as *AccountService) Logout(ctx context.Context, accountID uint) error {
	return as.accountRepository.Logout(ctx, accountID)
}

// ChangePassword 改密：验旧密码 → 哈希新密码 → 更新 → 双 token 全部作废
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
	return as.accountRepository.Logout(ctx, account.ID)
}

// Rename 改名：先签新 token（用新名字！），再和改名进同一事务，旧 token 立即失效
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
	return token, nil
}

// RefreshAccessToken 用 refreshToken 换新 accessToken 并覆盖旧 access token。
// 阶段1版本：FindAll 扫库比对；阶段7回填：先查 Redis 再回落扫库
func (as *AccountService) RefreshAccessToken(ctx context.Context, refreshToken string) (string, uint, string, error) {
	if refreshToken == "" {
		return "", 0, "", errors.New("refresh token is empty")
	}
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
