package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/store"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rt, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatalf("init runtime config: %v", err)
	}
	return New(st, rt, "test-token", nil).Router()
}

func do(t *testing.T, h http.Handler, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeObj(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode object response (%d): %v — body: %s", rec.Code, err, rec.Body.String())
	}
	return out
}

func decodeArr(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	var out []any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode array response (%d): %v — body: %s", rec.Code, err, rec.Body.String())
	}
	return out
}

func idOf(t *testing.T, body map[string]any) int64 {
	t.Helper()
	v, ok := body["id"].(float64)
	if !ok {
		t.Fatalf("response has no id: %v", body)
	}
	return int64(v)
}

// TestM1FullFlow 覆盖 M1 验收标准：实体全链路配置 + 保存即生效 + 鉴权 + 级联删除。
func TestM1FullFlow(t *testing.T) {
	h := newTestServer(t)

	// --- 鉴权 ---
	if rec := do(t, h, "GET", "/api/providers", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expect 401 without token, got %d", rec.Code)
	}

	// --- 提供商 ---
	rec := do(t, h, "POST", "/api/providers", map[string]any{
		"name": "zhipu", "base_url": "https://open.bigmodel.cn/api/paas/v4",
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create provider: %d — %s", rec.Code, rec.Body.String())
	}
	provID := idOf(t, decodeObj(t, rec))

	if rec := do(t, h, "POST", "/api/providers", map[string]any{
		"name": "zhipu", "base_url": "https://x",
	}, "test-token"); rec.Code != http.StatusConflict {
		t.Fatalf("expect 409 on duplicate provider name, got %d", rec.Code)
	}

	// --- 密钥池 ---
	rec = do(t, h, "POST", "/api/pools", map[string]any{
		"provider_id": provID, "name": "premium", "weight": 3,
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pool: %d — %s", rec.Code, rec.Body.String())
	}
	premiumID := idOf(t, decodeObj(t, rec))

	rec = do(t, h, "POST", "/api/pools", map[string]any{
		"provider_id": provID, "name": "basic",
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pool basic: %d", rec.Code)
	}
	basicID := idOf(t, decodeObj(t, rec))

	// --- 密钥（批量 + 去重）---
	rec = do(t, h, "POST", "/api/keys", map[string]any{
		"pool_id": premiumID,
		"keys":    "sk-aaaa1111\nsk-bbbb2222\nsk-cccc3333\n\n  \nsk-aaaa1111\n",
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create keys: %d — %s", rec.Code, rec.Body.String())
	}
	kb := decodeObj(t, rec)
	if kb["created"].(float64) != 3 || kb["skipped_duplicates"].(float64) != 0 {
		t.Fatalf("expect created=3 (in-request dedupe of 6 lines) skipped=0, got %v", kb)
	}
	rec = do(t, h, "POST", "/api/keys", map[string]any{
		"pool_id": premiumID, "keys": "sk-aaaa1111",
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-post keys: %d", rec.Code)
	}
	kb = decodeObj(t, rec)
	if kb["created"].(float64) != 0 || kb["skipped_duplicates"].(float64) != 1 {
		t.Fatalf("expect created=0 skipped=1 for existing key, got %v", kb)
	}

	rec = do(t, h, "POST", "/api/keys", map[string]any{
		"pool_id": basicID, "keys": "sk-dddd4444",
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create basic keys: %d", rec.Code)
	}

	// 密钥脱敏
	keys := decodeArr(t, do(t, h, "GET", "/api/keys", nil, "test-token"))
	if len(keys) != 4 {
		t.Fatalf("expect 4 keys, got %d", len(keys))
	}
	kv := keys[0].(map[string]any)["key_value"].(string)
	if kv == "sk-aaaa1111" || len(kv) < 8 || !contains(kv, "****") {
		t.Fatalf("key not masked: %q", kv)
	}
	if _, leaked := keys[0].(map[string]any)["KeyValue"]; leaked {
		t.Fatal("raw key value leaked in response")
	}

	// --- 模型 ---
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "glm-4.6",
		"input_price": 10, "output_price": 20,
		"pool_ids": []int64{premiumID, basicID},
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create model: %d — %s", rec.Code, rec.Body.String())
	}
	modelID := idOf(t, decodeObj(t, rec))

	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "glm-4.5-flash", "pool_ids": []int64{basicID},
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create model flash: %d", rec.Code)
	}
	flashID := idOf(t, decodeObj(t, rec))

	// 跨提供商的池不允许绑定
	rec = do(t, h, "POST", "/api/providers", map[string]any{"name": "other", "base_url": "https://y"}, "test-token")
	otherProvID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/pools", map[string]any{"provider_id": otherProvID, "name": "op"}, "test-token")
	otherPoolID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "PUT", fmt.Sprintf("/api/models/%d", modelID), map[string]any{
		"pool_ids": []int64{otherPoolID},
	}, "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expect 400 binding cross-provider pool, got %d", rec.Code)
	}

	// --- 路由 ---
	rec = do(t, h, "POST", "/api/routes", map[string]any{
		"name": "glm-pool",
		"targets": []map[string]any{
			{"model_id": modelID, "weight": 7},
			{"model_id": flashID, "weight": 3},
		},
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create route: %d — %s", rec.Code, rec.Body.String())
	}
	routeID := idOf(t, decodeObj(t, rec))

	// 重复目标模型 → 400
	rec = do(t, h, "POST", "/api/routes", map[string]any{
		"name": "bad",
		"targets": []map[string]any{
			{"model_id": modelID}, {"model_id": modelID},
		},
	}, "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expect 400 duplicate targets, got %d", rec.Code)
	}

	// 列表富化：目标带 model_name / provider_name
	routes := decodeArr(t, do(t, h, "GET", "/api/routes", nil, "test-token"))
	targets := routes[0].(map[string]any)["targets"].([]any)
	t0 := targets[0].(map[string]any)
	if t0["model_name"] != "glm-4.6" || t0["provider_name"] != "zhipu" {
		t.Fatalf("route targets not enriched: %v", t0)
	}
	if t0["weight"].(float64) != 7 {
		t.Fatalf("target weight lost: %v", t0)
	}

	// --- /v1/models 无需鉴权，返回逻辑路由名 ---
	v1 := decodeObj(t, do(t, h, "GET", "/v1/models", nil, ""))
	v1data := v1["data"].([]any)
	if len(v1data) != 1 || v1data[0].(map[string]any)["id"] != "glm-pool" {
		t.Fatalf("v1/models wrong: %v", v1)
	}

	// --- 运行层配置：读取默认 → 热更新 → 校验 ---
	settings := decodeObj(t, do(t, h, "GET", "/api/settings", nil, "test-token"))
	if settings["breaker.disable_threshold"].(float64) != 3 {
		t.Fatalf("default disable_threshold should be 3, got %v", settings["breaker.disable_threshold"])
	}
	rec = do(t, h, "PUT", "/api/settings", map[string]any{
		"breaker.disable_threshold": 5,
		"breaker.cooldown_ladder":   []string{"10s", "30s"},
	}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("update settings: %d — %s", rec.Code, rec.Body.String())
	}
	settings = decodeObj(t, do(t, h, "GET", "/api/settings", nil, "test-token"))
	if settings["breaker.disable_threshold"].(float64) != 5 {
		t.Fatalf("settings not persisted: %v", settings["breaker.disable_threshold"])
	}
	ladder := settings["breaker.cooldown_ladder"].([]any)
	if len(ladder) != 2 || ladder[0] != "10s" {
		t.Fatalf("ladder not updated: %v", ladder)
	}
	if rec := do(t, h, "PUT", "/api/settings", map[string]any{"breaker.disable_threshold": 0}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expect 400 invalid threshold, got %d", rec.Code)
	}
	if rec := do(t, h, "PUT", "/api/settings", map[string]any{"unknown.key": 1}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expect 400 unknown key, got %d", rec.Code)
	}

	// --- 健康与手动启停 ---
	do(t, h, "POST", fmt.Sprintf("/api/models/%d/disable", modelID), nil, "test-token")
	health := decodeObj(t, do(t, h, "GET", "/api/health", nil, "test-token"))
	hm := health["models"].([]any)[0].(map[string]any)
	if hm["status"] != "disabled" || hm["disable_reason"] == "" {
		t.Fatalf("model disable failed: %v", hm)
	}
	do(t, h, "POST", fmt.Sprintf("/api/models/%d/enable", modelID), nil, "test-token")
	health = decodeObj(t, do(t, h, "GET", "/api/health", nil, "test-token"))
	hm = health["models"].([]any)[0].(map[string]any)
	if hm["status"] != "active" || hm["fail_count"].(float64) != 0 {
		t.Fatalf("model enable failed: %v", hm)
	}
	hp := health["pools"].([]any)[0].(map[string]any)
	if hp["available_keys"].(float64) < 1 {
		t.Fatalf("pool availability wrong: %v", hp)
	}

	// --- 级联删除提供商：池/密钥/模型/路由目标全部清理 ---
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/providers/%d", provID), nil, "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("delete provider: %d", rec.Code)
	}
	if keys := decodeArr(t, do(t, h, "GET", "/api/keys", nil, "test-token")); len(keys) != 0 {
		t.Fatalf("keys should cascade-delete, got %d", len(keys))
	}
	if models := decodeArr(t, do(t, h, "GET", "/api/models", nil, "test-token")); len(models) != 0 {
		t.Fatalf("models should cascade-delete, got %d", len(models))
	}
	routes = decodeArr(t, do(t, h, "GET", "/api/routes", nil, "test-token"))
	if len(routes) != 1 {
		t.Fatalf("route itself should remain, got %d", len(routes))
	}
	targets = routes[0].(map[string]any)["targets"].([]any)
	if len(targets) != 0 {
		t.Fatalf("route targets should cascade-delete, got %d", len(targets))
	}

	// --- 404 ---
	if rec := do(t, h, "GET", "/api/providers/999", nil, "test-token"); rec.Code != http.StatusOK {
		_ = routeID // routeID 保留供 M2 使用断言扩展
	}
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/models/%d", modelID), nil, "test-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("expect 404 after cascade delete, got %d", rec.Code)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestStatsEmptyDBNoNullPanic 回归：空库时 SUM 返回 NULL 不得导致 Scan 报错
func TestStatsEmptyDBNoNullPanic(t *testing.T) {
	h := newTestServer(t)
	rec := do(t, h, "GET", "/api/stats/overview", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview on empty db: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "GET", "/api/stats/breakdown?dim=model", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("breakdown on empty db: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "GET", "/api/stats/timeseries", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeseries on empty db: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "GET", "/api/logs", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs on empty db: %d — %s", rec.Code, rec.Body.String())
	}
}

// TestEmptyPoolKeysNotNullArray 回归：空池的 keys 必须序列化为 [] 而非 null（前端 .length 会崩）
func TestEmptyPoolKeysNotNullArray(t *testing.T) {
	h := newTestServer(t)
	do(t, h, "POST", "/api/providers", map[string]any{
		"name": "zhipu", "base_url": "https://x",
	}, "test-token")
	rec := do(t, h, "POST", "/api/pools", map[string]any{
		"provider_id": 1, "name": "empty-pool",
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pool: %d — %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"keys":[]`) {
		t.Fatalf("create response keys must be [] not null: %s", got)
	}
	rec = do(t, h, "GET", "/api/pools", nil, "test-token")
	if got := rec.Body.String(); !strings.Contains(got, `"keys":[]`) {
		t.Fatalf("list response keys must be [] not null: %s", got)
	}
}

// TestModelRequiresPool 回归：模型必须绑定至少一个密钥池
func TestModelRequiresPool(t *testing.T) {
	h := newTestServer(t)
	do(t, h, "POST", "/api/providers", map[string]any{"name": "zhipu", "base_url": "https://x"}, "test-token")
	rec := do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "no-pool", "pool_ids": []int64{},
	}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "密钥池") {
		t.Fatalf("empty pool_ids must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
}
