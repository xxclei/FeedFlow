package db

import (
	"fmt"

	"myfeed/internal/account"
	"myfeed/internal/config"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"myfeed/internal/video"
)

// NewDB 拼 DSN 并建立连接，返回 GORM 句柄（内部是连接池）
func NewDB(cfg config.DatabaseConfig) (*gorm.DB, error) {
	dsn :=
		fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
			cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DBName)

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	return db, nil
}

// AutoMigrate 把结构体同步成真实的表
func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&account.Account{},
		&video.Video{}, &video.Tag{}, &video.VideoTag{}, &video.OutboxMsg{},
	)
}

// CloseDB 关闭底层连接池
func CloseDB(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
