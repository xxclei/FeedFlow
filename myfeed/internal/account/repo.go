package account

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// ErrUsernameTaken 用户名已被占用（由 MySQL 1062 唯一键冲突翻译而来）
var ErrUsernameTaken = errors.New("username already taken")

// AccountRepository 只负责读写 accounts 表，不懂业务（不碰 bcrypt/JWT/gin）
type AccountRepository struct {
	db *gorm.DB
} // 依赖注入：只声明自己需要什么，其他函数进行填充，本质解耦

func NewAccountRepository(db *gorm.DB) *AccountRepository {
	return &AccountRepository{db: db}
} // 这里连接数据库交给其他文件完成

// CreateAccount 注册。并发同名插入靠数据库唯一键兜底，
// 这里捕获 1062 翻译成业务错误（不要"先查再插"，有并发窗口）
func (ar *AccountRepository) CreateAccount(ctx context.Context, account *Account) error {
	err := ar.db.WithContext(ctx).Create(account).Error
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return ErrUsernameTaken
		}
	}
	return err
}

// FindByID 按主键查。查不到时 GORM 返回 gorm.ErrRecordNotFound，原样上抛
func (ar *AccountRepository) FindByID(ctx context.Context, id uint) (*Account, error) {
	var account Account
	if err := ar.db.WithContext(ctx).First(&account, id).Error; err != nil {
		return nil, err
	}
	return &account, nil
}

// FindByUsername 登录/改密时用用户名定位用户
func (ar *AccountRepository) FindByUsername(ctx context.Context, username string) (*Account, error) {
	var account Account
	if err := ar.db.WithContext(ctx).Where("username = ?", username).First(&account).Error; err != nil {
		return nil, err
	}
	return &account, nil
}

// UpdateToken 登录成功后覆盖旧 token（顶号：旧设备立刻下线）
func (ar *AccountRepository) UpdateToken(ctx context.Context, id uint, token string) error {
	return ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Update("token", token).Error
}

// Login 登录成功后覆盖双 token（顶号：旧设备全下线）
func (ar *AccountRepository) Login(ctx context.Context, id uint, token, refreshToken string) error {
	result := ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Updates(map[string]interface{}{
		"token":         token,
		"refresh_token": refreshToken,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// RenameWithToken 改名 + 写新 token 必须在同一事务：
// "名字换了且旧token同时作废"要么全发生，要么全不发生
func (ar *AccountRepository) RenameWithToken(ctx context.Context, id uint, newUsername string, token string) error {
	return ar.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Account{}).Where("id = ?", id).Update("username", newUsername)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return tx.Model(&Account{}).Where("id = ?", id).Update("token", token).Error
	})
}

// ChangePassword 更新密码哈希
func (ar *AccountRepository) ChangePassword(ctx context.Context, id uint, newPassword string) error {
	result := ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Update("password", newPassword)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Logout 清空双 token（登出 / 改密后强制全部下线）
func (ar *AccountRepository) Logout(ctx context.Context, id uint) error {
	result := ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Updates(map[string]interface{}{
		"token":         "",
		"refresh_token": "",
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UpdateAvatar 头像上传后写 URL
func (ar *AccountRepository) UpdateAvatar(ctx context.Context, accountID uint, avatarURL string) error {
	return ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", accountID).Update("avatar_url", avatarURL).Error
}

// UpdateFields 按需更新任意字段（bio / avatar_url）
func (ar *AccountRepository) UpdateFields(ctx context.Context, id uint, updates map[string]interface{}) error {
	return ar.db.WithContext(ctx).Model(&Account{}).Where("id = ?", id).Updates(updates).Error
}

// FindAll 全量账号（阶段1 refresh 扫库用；阶段7 有缓存后仍是兜底路径）
func (ar *AccountRepository) FindAll(ctx context.Context) ([]*Account, error) {
	var accounts []*Account
	if err := ar.db.WithContext(ctx).Find(&accounts).Error; err != nil {
		return nil, err
	}
	return accounts, nil
}
