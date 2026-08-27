package store

import (
	"fmt"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// Store 包装 GORM DB 句柄。
type Store struct {
	DB *gorm.DB
}

// Open 打开（必要时创建）SQLite 数据库并执行迁移。
func Open(path string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(4)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if execErr := db.Exec(pragma).Error; execErr != nil {
			return nil, fmt.Errorf("exec %q: %w", pragma, execErr)
		}
	}
	if err := db.AutoMigrate(
		&Provider{}, &KeyPool{}, &ApiKey{}, &Model{}, &ModelPool{},
		&Route{}, &RouteTarget{}, &AppConfig{}, &RequestLog{}, &RequestAttempt{}, &ContentLog{},
		&RequestLogDaily{},
	); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db}, nil
}
