package store

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestOpenMigratesLegacyContentLog：旧库 content_log 没有客户端入站列时，
// Open 必须能加上这两列，而不是被 SQLite「NOT NULL 无 DEFAULT」拦住。
func TestOpenMigratesLegacyContentLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := gorm.Open(sqlite.Open(path+"?_pragma=busy_timeout(5000)&mode=rwc"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if err := raw.Exec(`
CREATE TABLE content_log (
  request_id TEXT PRIMARY KEY,
  route TEXT NOT NULL,
  request_headers TEXT NOT NULL DEFAULT '',
  request_body TEXT NOT NULL,
  response_headers TEXT NOT NULL DEFAULT '',
  response_body TEXT NOT NULL,
  created_at INTEGER
)`).Error; err != nil {
		t.Fatalf("create legacy content_log: %v", err)
	}
	if err := raw.Exec(`INSERT INTO content_log (request_id, route, request_headers, request_body, response_headers, response_body, created_at)
VALUES ('old-1', 'glm', 'Content-Type: application/json', '{"model":"glm"}', '', '{"ok":true}', 1)`).Error; err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	sqlDB, err := raw.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if !st.DB.Migrator().HasColumn(&ContentLog{}, "client_request_headers") {
		t.Fatal("client_request_headers missing after Open")
	}
	if !st.DB.Migrator().HasColumn(&ContentLog{}, "client_request_body") {
		t.Fatal("client_request_body missing after Open")
	}

	var cl ContentLog
	if err := st.DB.Where("request_id = ?", "old-1").First(&cl).Error; err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if cl.RequestBody != `{"model":"glm"}` || cl.ResponseBody != `{"ok":true}` {
		t.Fatalf("legacy bodies lost: %+v", cl)
	}
	if cl.ClientRequestHeaders != "" || cl.ClientRequestBody != "" {
		t.Fatalf("new client columns should default to empty, got headers=%q body=%q", cl.ClientRequestHeaders, cl.ClientRequestBody)
	}
}

// TestOpenMigratesHalfMigratedContentLog：headers 列已加上、body 列因无 DEFAULT 失败时，
// 再次 Open 必须只补 client_request_body，不能把已有行打坏。
func TestOpenMigratesHalfMigratedContentLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "half.db")
	raw, err := gorm.Open(sqlite.Open(path+"?_pragma=busy_timeout(5000)&mode=rwc"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if err := raw.Exec(`
CREATE TABLE content_log (
  request_id TEXT PRIMARY KEY,
  route TEXT NOT NULL,
  request_headers TEXT NOT NULL DEFAULT '',
  request_body TEXT NOT NULL,
  response_headers TEXT NOT NULL DEFAULT '',
  response_body TEXT NOT NULL,
  created_at INTEGER,
  client_request_headers TEXT NOT NULL DEFAULT ''
)`).Error; err != nil {
		t.Fatalf("create half-migrated content_log: %v", err)
	}
	if err := raw.Exec(`INSERT INTO content_log (request_id, route, request_headers, request_body, response_headers, response_body, created_at, client_request_headers)
VALUES ('old-1', 'glm', 'Content-Type: application/json', '{"model":"glm"}', '', '{"ok":true}', 1, 'Authorization: Bearer s****')`).Error; err != nil {
		t.Fatalf("seed half-migrated row: %v", err)
	}
	sqlDB, err := raw.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open half-migrated db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if !st.DB.Migrator().HasColumn(&ContentLog{}, "client_request_body") {
		t.Fatal("client_request_body missing after Open")
	}
	var cl ContentLog
	if err := st.DB.Where("request_id = ?", "old-1").First(&cl).Error; err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if cl.ClientRequestHeaders != "Authorization: Bearer s****" {
		t.Fatalf("existing client headers lost: %q", cl.ClientRequestHeaders)
	}
	if cl.ClientRequestBody != "" {
		t.Fatalf("new body column should default to empty, got %q", cl.ClientRequestBody)
	}
}

func TestOpenCreatesContentLogOnFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open fresh db: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if !st.DB.Migrator().HasTable(&ContentLog{}) {
		t.Fatal("content_log missing on fresh db")
	}
	for _, col := range []string{"client_request_headers", "client_request_body", "request_headers", "request_body", "response_headers", "response_body"} {
		if !st.DB.Migrator().HasColumn(&ContentLog{}, col) {
			t.Fatalf("column %s missing on fresh db", col)
		}
	}
	row := ContentLog{RequestID: "n1", Route: "r", ClientRequestBody: `{"a":1}`, RequestBody: `{"b":2}`, ResponseBody: `{"c":3}`}
	if err := st.DB.Create(&row).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
}
