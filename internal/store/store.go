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
//
// synchronous=NORMAL：WAL 模式下提交不再逐条 fsync（仅 checkpoint 时落盘），
// 进程崩溃不丢已提交事务，仅断电可能丢最近若干提交——对本地统计库是可接受的取舍，
// 高并发写入路径上这一项消除的是每请求多次 fsync 的最大开销。
func Open(path string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)&mode=rwc"), &gorm.Config{
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
	// vk_rate_limits（旧版 DB 落库限流计数）已废弃：限流改为进程内固定分钟窗口，
	// 见 internal/api/vk_limiter.go；表中数据无消费方，直接删除。
	if err := dropLegacyVKRateLimits(db); err != nil {
		return nil, fmt.Errorf("drop legacy vk_rate_limits: %w", err)
	}
	// content_log 走手工迁移：GORM AutoMigrate 对 SQLite 改列会整表重建，
	// 正文表 + 大 WAL 会把启动卡死（start.sh 15s 健康检查失败）。
	if err := migrateContentLog(db); err != nil {
		return nil, fmt.Errorf("migrate content_log: %w", err)
	}
	if err := db.AutoMigrate(
		&Provider{}, &ApiKey{}, &Model{}, &ModelKey{}, &ModelKeyBan{},
		&Route{}, &RouteTarget{}, &AppConfig{}, &RequestLog{}, &RequestAttempt{},
		&RequestLogDaily{},
		&VirtualKey{},
		&MCPBackend{}, &RouteMcpTarget{},
	); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := normalizeVKAllowedRoutes(db); err != nil {
		return nil, fmt.Errorf("normalize vk allowed_routes: %w", err)
	}
	if err := migrateEndpointColumn(db); err != nil {
		return nil, fmt.Errorf("migrate endpoint column: %w", err)
	}
	if err := migrateProtocolRenameAndFields(db); err != nil {
		return nil, fmt.Errorf("migrate protocol rename and fields: %w", err)
	}
	return &Store{DB: db}, nil
}

// dropLegacyVKRateLimits 删除旧版 DB 落库限流计数表（幂等；新库无此表直接跳过）。
func dropLegacyVKRateLimits(db *gorm.DB) error {
	if !db.Migrator().HasTable("vk_rate_limits") {
		return nil
	}
	return db.Migrator().DropTable("vk_rate_limits")
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

// migrateProtocolRenameAndFields 添加 api_path 和 body_override 字段，并重命名协议和端点值
func migrateProtocolRenameAndFields(db *gorm.DB) error {
	// 添加 api_path 到 model 表
	if !db.Migrator().HasColumn(&Model{}, "api_path") {
		if err := db.Exec(`ALTER TABLE model ADD COLUMN api_path VARCHAR(512) NOT NULL DEFAULT ''`).Error; err != nil {
			return err
		}
	}
	// 添加 body_override 到 model 表
	if !db.Migrator().HasColumn(&Model{}, "body_override") {
		if err := db.Exec(`ALTER TABLE model ADD COLUMN body_override TEXT NOT NULL DEFAULT ''`).Error; err != nil {
			return err
		}
	}
	// 添加 body_override 到 route 表
	if !db.Migrator().HasColumn(&Route{}, "body_override") {
		if err := db.Exec(`ALTER TABLE route ADD COLUMN body_override TEXT NOT NULL DEFAULT ''`).Error; err != nil {
			return err
		}
	}

	// 重命名 model 表的协议值: openai → completions, anthropic → messages
	if err := db.Exec(`UPDATE model SET protocol = 'completions' WHERE protocol = 'openai'`).Error; err != nil {
		return err
	}
	if err := db.Exec(`UPDATE model SET protocol = 'messages' WHERE protocol = 'anthropic'`).Error; err != nil {
		return err
	}

	// 重命名 route 表的端点值: chat → completions
	if err := db.Exec(`UPDATE route SET endpoint = 'completions' WHERE endpoint = 'chat'`).Error; err != nil {
		return err
	}

	return nil
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

// migrateContentLog 建表或给旧表补列。不加进 AutoMigrate：
// glebarez/sqlite 改列会 recreateTable，content_log 正文大、WAL 大时启动会卡死。
func migrateContentLog(db *gorm.DB) error {
	if !db.Migrator().HasTable(&ContentLog{}) {
		if err := db.Exec(`
CREATE TABLE content_log (
  request_id TEXT PRIMARY KEY,
  route TEXT NOT NULL,
  client_request_headers TEXT NOT NULL DEFAULT '',
  client_request_body TEXT NOT NULL DEFAULT '',
  request_headers TEXT NOT NULL DEFAULT '',
  request_body TEXT NOT NULL DEFAULT '',
  response_headers TEXT NOT NULL DEFAULT '',
  response_body TEXT NOT NULL DEFAULT '',
  created_at INTEGER
)`).Error; err != nil {
			return err
		}
		return db.Exec(`CREATE INDEX IF NOT EXISTS idx_cl_time ON content_log(created_at)`).Error
	}
	type col struct {
		name string
		ddl  string
	}
	for _, c := range []col{
		{"request_headers", `ALTER TABLE content_log ADD COLUMN request_headers TEXT NOT NULL DEFAULT ''`},
		{"response_headers", `ALTER TABLE content_log ADD COLUMN response_headers TEXT NOT NULL DEFAULT ''`},
		{"client_request_headers", `ALTER TABLE content_log ADD COLUMN client_request_headers TEXT NOT NULL DEFAULT ''`},
		{"client_request_body", `ALTER TABLE content_log ADD COLUMN client_request_body TEXT NOT NULL DEFAULT ''`},
	} {
		if db.Migrator().HasColumn(&ContentLog{}, c.name) {
			continue
		}
		if err := db.Exec(c.ddl).Error; err != nil {
			return err
		}
	}
	return db.Exec(`CREATE INDEX IF NOT EXISTS idx_cl_time ON content_log(created_at)`).Error
}
