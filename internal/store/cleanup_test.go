package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

func openCleanupStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countRows(t *testing.T, st *store.Store, table string) int64 {
	t.Helper()
	var n int64
	if err := st.DB.Table(table).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPurgeRetentions(t *testing.T) {
	st := openCleanupStore(t)
	now := time.Now().Unix()
	old := now - 10*86400
	fresh := now - 2*86400

	logs := []store.RequestLog{
		{RequestID: "old-1", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: old},
		{RequestID: "old-2", Route: "r", Model: "m", Provider: "p", Status: "error", ErrorCode: "timeout", CreatedAt: old},
		{RequestID: "new-1", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: fresh},
	}
	for i := range logs {
		if err := st.DB.Create(&logs[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	attempts := []store.RequestAttempt{
		{RequestID: "old-1", Attempt: 1, Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: old},
		{RequestID: "new-1", Attempt: 1, Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: fresh},
	}
	for i := range attempts {
		if err := st.DB.Create(&attempts[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	caps := []store.ContentLog{
		{RequestID: "old-1", Route: "r", RequestBody: "{}", ResponseBody: "{}", CreatedAt: old},
		{RequestID: "new-1", Route: "r", RequestBody: "{}", ResponseBody: "{}", CreatedAt: fresh},
	}
	for i := range caps {
		if err := st.DB.Create(&caps[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	daily := []store.RequestLogDaily{
		{Day: store.DayKey(old), Route: "r", Model: "m", Provider: "p", Status: "success", Total: 5},
		{Day: store.DayKey(now), Route: "r", Model: "m", Provider: "p", Status: "success", Total: 9},
	}
	for i := range daily {
		if err := st.DB.Create(&daily[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := store.PurgeRetentions(st.DB, 5, 7)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted["request_log"] != 2 || deleted["request_attempt"] != 1 ||
		deleted["request_log_daily"] != 1 || deleted["content_log"] != 1 {
		t.Fatalf("deleted counts wrong: %v", deleted)
	}
	if n := countRows(t, st, "request_log"); n != 1 {
		t.Fatalf("request_log remaining %d, want 1", n)
	}
	if n := countRows(t, st, "request_attempt"); n != 1 {
		t.Fatalf("request_attempt remaining %d, want 1", n)
	}
	if n := countRows(t, st, "content_log"); n != 1 {
		t.Fatalf("content_log remaining %d, want 1", n)
	}
	if n := countRows(t, st, "request_log_daily"); n != 1 {
		t.Fatalf("request_log_daily remaining %d, want 1", n)
	}
}

func TestPurgeRetentionsZeroDisables(t *testing.T) {
	st := openCleanupStore(t)
	old := time.Now().Unix() - 365*86400
	if err := st.DB.Create(&store.RequestLog{
		RequestID: "ancient", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ContentLog{
		RequestID: "ancient", Route: "r", RequestBody: "{}", ResponseBody: "{}", CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}

	deleted, err := store.PurgeRetentions(st.DB, 0, 0)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("retention 0 must delete nothing: %v", deleted)
	}
	if n := countRows(t, st, "request_log"); n != 1 {
		t.Fatalf("request_log remaining %d, want 1", n)
	}
	if n := countRows(t, st, "content_log"); n != 1 {
		t.Fatalf("content_log remaining %d, want 1", n)
	}
}

func TestClearStats(t *testing.T) {
	st := openCleanupStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "a", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now},
		{RequestID: "b", Route: "r", Model: "m", Provider: "p", Status: "error", ErrorCode: "500", CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB.Create(&store.RequestAttempt{
		RequestID: "a", Attempt: 1, Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: store.DayKey(now), Route: "r", Model: "m", Provider: "p", Status: "success", Total: 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ContentLog{
		RequestID: "a", Route: "r", RequestBody: "{}", ResponseBody: "{}", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	cleared, err := store.ClearStats(st.DB)
	if err != nil {
		t.Fatalf("clear stats: %v", err)
	}
	if cleared["request_log"] != 2 || cleared["request_attempt"] != 1 || cleared["request_log_daily"] != 1 {
		t.Fatalf("cleared counts wrong: %v", cleared)
	}
	if n := countRows(t, st, "content_log"); n != 1 {
		t.Fatalf("clear-stats must keep content_log, remaining %d", n)
	}
}
