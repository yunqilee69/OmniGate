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
	db, err := gorm.Open(sqlite.Open(path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)&mode=rwc"), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(4)
	}
	if err := migratePoolsAway(db); err != nil {
		return nil, fmt.Errorf("migrate key pools away: %w", err)
	}
	if err := db.AutoMigrate(
		&Provider{}, &ApiKey{}, &Model{}, &ModelKey{}, &ModelKeyBan{},
		&Route{}, &RouteTarget{}, &AppConfig{}, &RequestLog{}, &RequestAttempt{}, &ContentLog{},
		&RequestLogDaily{},
	); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrateEndpointColumn(db); err != nil {
		return nil, fmt.Errorf("migrate endpoint column: %w", err)
	}
	return &Store{DB: db}, nil
}

// Close 关闭底层连接池(同时触发 WAL checkpoint 并清理 -wal/-shm 副产物)。
// 未关闭的句柄在 Windows 上会阻止 t.TempDir 与数据目录删除。
func (s *Store) Close() error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func migrateEndpointColumn(db *gorm.DB) error {
	if db.Migrator().HasColumn(&Route{}, "endpoint") {
		return nil
	}
	if err := db.Exec(`ALTER TABLE route ADD COLUMN endpoint TEXT NOT NULL DEFAULT 'chat'`).Error; err != nil {
		return err
	}
	return db.Exec(`UPDATE route SET endpoint = 'chat' WHERE endpoint = ''`).Error
}

// migratePoolsAway 把旧版“密钥池”结构迁移为模型直绑密钥：
// api_key.provider_id 从池回填，model_pool×池内 key 展开 成 model_key，然后删除池相关表。
// 幂等：新库（无 key_pool 表）直接跳过。
func migratePoolsAway(db *gorm.DB) error {
	if !db.Migrator().HasTable("key_pool") {
		return nil
	}
	if db.Migrator().HasColumn(&ApiKey{}, "provider_id") {
		if err := db.Exec(`UPDATE api_key SET provider_id = (
			SELECT provider_id FROM key_pool WHERE key_pool.id = api_key.pool_id
		) WHERE provider_id IS NULL OR provider_id = 0`).Error; err != nil {
			return err
		}
	} else {
		if err := db.Exec(`ALTER TABLE api_key ADD COLUMN provider_id INTEGER`).Error; err != nil {
			return err
		}
		if err := db.Exec(`UPDATE api_key SET provider_id = (
			SELECT provider_id FROM key_pool WHERE key_pool.id = api_key.pool_id
		)`).Error; err != nil {
			return err
		}
	}
	if db.Migrator().HasTable("model_pool") {
		if err := db.Exec(`CREATE TABLE IF NOT EXISTS model_key (
			model_id INTEGER NOT NULL, key_id INTEGER NOT NULL, PRIMARY KEY (model_id, key_id)
		)`).Error; err != nil {
			return err
		}
		if err := db.Exec(`INSERT OR IGNORE INTO model_key (model_id, key_id)
			SELECT DISTINCT mp.model_id, k.id
			FROM model_pool mp
			JOIN key_pool p ON p.id = mp.pool_id
			JOIN api_key k ON k.pool_id = p.id`).Error; err != nil {
			return err
		}
		if err := db.Exec(`DROP TABLE model_pool`).Error; err != nil {
			return err
		}
	}
	return db.Exec(`DROP TABLE key_pool`).Error
}
