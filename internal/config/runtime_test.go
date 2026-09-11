package config

import (
	"encoding/json"
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

// TestFallbackDefaults 校验兜底模型默认值：每个端点各一个键，默认全部未配置。
func TestFallbackDefaults(t *testing.T) {
	rtm, err := NewRuntimeManager(newRuntimeStore(t))
	if err != nil {
		t.Fatalf("new runtime manager: %v", err)
	}
	rt := rtm.Snapshot()
	if rt.FallbackEnabled {
		t.Fatalf("FallbackEnabled = true, want false")
	}
	for _, ep := range fallbackEndpoints {
		if got := rt.FallbackModels[ep]; got != 0 {
			t.Errorf("FallbackModels[%s] = %d, want 0", ep, got)
		}
	}
}

// TestFallbackPerEndpoint 校验各端点兜底模型可独立配置并即时热生效。
func TestFallbackPerEndpoint(t *testing.T) {
	rtm, err := NewRuntimeManager(newRuntimeStore(t))
	if err != nil {
		t.Fatalf("new runtime manager: %v", err)
	}
	payload := map[string]json.RawMessage{
		"fallback.enabled":              json.RawMessage(`true`),
		"fallback.completions_model_id": json.RawMessage(`11`),
		"fallback.messages_model_id":    json.RawMessage(`22`),
		"fallback.responses_model_id":   json.RawMessage(`33`),
		"fallback.embedding_model_id":   json.RawMessage(`44`),
		"fallback.rerank_model_id":      json.RawMessage(`55`),
	}
	if err := rtm.Update(payload); err != nil {
		t.Fatalf("update: %v", err)
	}

	want := map[string]int64{
		"completions": 11,
		"messages":    22,
		"responses":   33,
		"embedding":   44,
		"rerank":      55,
	}
	rt := rtm.Snapshot()
	if !rt.FallbackEnabled {
		t.Fatalf("FallbackEnabled = false, want true")
	}
	for ep, id := range want {
		if got := rt.FallbackModels[ep]; got != id {
			t.Errorf("FallbackModels[%s] = %d, want %d", ep, got, id)
		}
	}

	// 未被支持的端点（如 image）不参与兜底，查表得 0。
	if got := rt.FallbackModels["image"]; got != 0 {
		t.Errorf("FallbackModels[image] = %d, want 0", got)
	}

	// 重新打开（模拟重启）后配置仍在。
	rtm2, err := NewRuntimeManager(rtm.db)
	if err != nil {
		t.Fatalf("reopen runtime manager: %v", err)
	}
	if got := rtm2.Snapshot().FallbackModels["rerank"]; got != 55 {
		t.Errorf("after reopen FallbackModels[rerank] = %d, want 55", got)
	}
}

// TestFallbackModelIDValidation 校验非法兜底模型 ID 被拒绝，不改动既有配置。
func TestFallbackModelIDValidation(t *testing.T) {
	rtm, err := NewRuntimeManager(newRuntimeStore(t))
	if err != nil {
		t.Fatalf("new runtime manager: %v", err)
	}
	if err := rtm.Update(map[string]json.RawMessage{
		"fallback.embedding_model_id": json.RawMessage(`-1`),
	}); err == nil {
		t.Fatalf("negative model id accepted, want validation error")
	}
	if err := rtm.Update(map[string]json.RawMessage{
		"fallback.embedding_model_id": json.RawMessage(`"nope"`),
	}); err == nil {
		t.Fatalf("string model id accepted, want validation error")
	}
}

// TestMigrateLegacyFallbackModelID 校验旧的单值 fallback.model_id 迁移到 completions 兜底。
func TestMigrateLegacyFallbackModelID(t *testing.T) {
	st := newRuntimeStore(t)
	// 先建 manager 播种默认键，再模拟旧版本写入遗留键。
	if _, err := NewRuntimeManager(st); err != nil {
		t.Fatalf("seed runtime manager: %v", err)
	}
	if err := st.DB.Create(&store.AppConfig{Key: "fallback.model_id", Value: `42`}).Error; err != nil {
		t.Fatalf("seed legacy key: %v", err)
	}

	rtm, err := NewRuntimeManager(st)
	if err != nil {
		t.Fatalf("reopen runtime manager: %v", err)
	}
	if got := rtm.Snapshot().FallbackModels["completions"]; got != 42 {
		t.Errorf("FallbackModels[completions] = %d, want 42", got)
	}
	var legacy int64
	if err := st.DB.Model(&store.AppConfig{}).Where("key = ?", "fallback.model_id").Count(&legacy).Error; err != nil {
		t.Fatalf("count legacy key: %v", err)
	}
	if legacy != 0 {
		t.Errorf("legacy fallback.model_id rows = %d, want 0", legacy)
	}
}
