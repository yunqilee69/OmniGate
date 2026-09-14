package config

import (
	"path/filepath"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

func newRuntimeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestLegacyFallbackKeysRemoved 兜底配置已下沉到路由级：启动时必须清掉历史全局键
// （fallback.model_id / fallback.enabled / fallback.<endpoint>_model_id），
// GET /api/settings 不得再暴露它们，Update 也不得接受它们。
func TestLegacyFallbackKeysRemoved(t *testing.T) {
	st := newRuntimeStore(t)
	if _, err := NewRuntimeManager(st); err != nil {
		t.Fatalf("seed runtime manager: %v", err)
	}
	for _, kv := range [][2]string{
		{"fallback.model_id", `42`},
		{"fallback.enabled", `true`},
		{"fallback.completions_model_id", `11`},
		{"fallback.messages_model_id", `22`},
		{"fallback.responses_model_id", `33`},
		{"fallback.embedding_model_id", `44`},
		{"fallback.rerank_model_id", `55`},
	} {
		if err := st.DB.Create(&store.AppConfig{Key: kv[0], Value: kv[1]}).Error; err != nil {
			t.Fatalf("seed %s: %v", kv[0], err)
		}
	}

	rtm, err := NewRuntimeManager(st)
	if err != nil {
		t.Fatalf("reopen runtime manager: %v", err)
	}

	var n int64
	if err := st.DB.Model(&store.AppConfig{}).Where("key LIKE ?", "fallback.%").Count(&n).Error; err != nil {
		t.Fatalf("count fallback keys: %v", err)
	}
	if n != 0 {
		t.Errorf("legacy fallback rows = %d, want 0", n)
	}

	all, err := rtm.All()
	if err != nil {
		t.Fatalf("all settings: %v", err)
	}
	for k := range all {
		if len(k) >= 9 && k[:9] == "fallback." {
			t.Errorf("settings expose legacy key %q", k)
		}
	}
}
