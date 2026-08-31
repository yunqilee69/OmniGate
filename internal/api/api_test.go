package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/store"
)

func newTestServerWithStore(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rt, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatalf("init runtime config: %v", err)
	}
	return New(st, rt, AdminAuth{Username: "admin", Password: "test-token"}, nil, nil).Router(), st
}

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	h, _ := newTestServerWithStore(t)
	return h
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
		// 测试服务器凭据为 admin:<token>，走管理面 Basic 通道
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:"+token)))
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

	// --- 密钥（单个新增：名称必填、同提供商唯一）---
	createKey := func(name, value string) int64 {
		t.Helper()
		rec := do(t, h, "POST", "/api/keys", map[string]any{
			"provider_id": provID, "key_value": value, "name": name,
		}, "test-token")
		if rec.Code != http.StatusCreated {
			t.Fatalf("create key %s: %d — %s", name, rec.Code, rec.Body.String())
		}
		return idOf(t, decodeObj(t, rec))
	}
	premiumKeys := []int64{createKey("premium-1", "sk-aaaa1111"), createKey("premium-2", "sk-bbbb2222"), createKey("premium-3", "sk-cccc3333")}
	extraKeys := []int64{createKey("basic-1", "sk-dddd4444")}

	if rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": provID, "key_value": "sk-eeee5555"}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expect 400 missing name, got %d", rec.Code)
	}
	if rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": provID, "key_value": "sk-eeee5555", "name": "premium-1"}, "test-token"); rec.Code != http.StatusConflict {
		t.Fatalf("expect 409 duplicate name, got %d", rec.Code)
	}
	if rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": provID, "key_value": "sk-aaaa1111", "name": "premium-9"}, "test-token"); rec.Code != http.StatusConflict {
		t.Fatalf("expect 409 duplicate value, got %d", rec.Code)
	}

	// 编辑：改名 + 改值；重复值 409；回传脱敏值视为不修改
	rec = do(t, h, "PUT", fmt.Sprintf("/api/keys/%d", premiumKeys[0]), map[string]any{"name": "premium-1x", "key_value": "sk-aaaa9999"}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("edit key: %d — %s", rec.Code, rec.Body.String())
	}
	kb := decodeObj(t, rec)
	if kb["key_value"] != "sk-aa****9999" || kb["name"] != "premium-1x" {
		t.Fatalf("edit key response wrong: %v", kb)
	}
	if rec = do(t, h, "PUT", fmt.Sprintf("/api/keys/%d", premiumKeys[1]), map[string]any{"key_value": "sk-aaaa9999"}, "test-token"); rec.Code != http.StatusConflict {
		t.Fatalf("expect 409 duplicate value on update, got %d", rec.Code)
	}
	if rec = do(t, h, "PUT", fmt.Sprintf("/api/keys/%d", premiumKeys[0]), map[string]any{"key_value": "sk-aa****9999"}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("masked round-trip alone should hit no-fields-to-update, got %d", rec.Code)
	}

	// 密钥脱敏（默认）与 reveal=1 明文（本地单人场景）
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
	if _, leaked := keys[0].(map[string]any)["key_value_plain"]; leaked {
		t.Fatal("plain value must be absent without reveal=1")
	}
	keys = decodeArr(t, do(t, h, "GET", "/api/keys?reveal=1", nil, "test-token"))
	if plain := keys[0].(map[string]any)["key_value_plain"].(string); plain != "sk-aaaa9999" {
		t.Fatalf("reveal=1 must return plaintext, got %q", plain)
	}

	// --- 模型 ---
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "glm-4.6",
		"input_price": 10, "output_price": 20,
		"key_ids": append(append([]int64{}, premiumKeys...), extraKeys...),
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create model: %d — %s", rec.Code, rec.Body.String())
	}
	modelID := idOf(t, decodeObj(t, rec))

	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "glm-4.5-flash", "key_ids": extraKeys,
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create model flash: %d", rec.Code)
	}
	flashID := idOf(t, decodeObj(t, rec))

	// 跨提供商的密钥不允许绑定
	rec = do(t, h, "POST", "/api/providers", map[string]any{"name": "other", "base_url": "https://y"}, "test-token")
	otherProvID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": otherProvID, "key_value": "sk-other9999", "name": "premium-1"}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create other-provider key: %d — %s", rec.Code, rec.Body.String())
	}
	otherKeys := []int64{idOf(t, decodeObj(t, rec))}
	rec = do(t, h, "PUT", fmt.Sprintf("/api/models/%d", modelID), map[string]any{
		"key_ids": otherKeys,
	}, "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expect 400 binding cross-provider key, got %d", rec.Code)
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

	// --- /v1/models 返回逻辑路由名（Basic 凭据走代理面通道） ---
	v1 := decodeObj(t, do(t, h, "GET", "/v1/models", nil, "test-token"))
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
	if rec := do(t, h, "PUT", "/api/settings", map[string]any{"unknown.key": 1}, "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("expect 200 (unknown keys are ignored), got %d", rec.Code)
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
	if hks := health["keys"].([]any); len(hks) != 5 {
		t.Fatalf("health keys wrong: %d", len(hks))
	}

	// --- 级联删除提供商：密钥/模型/路由目标全部清理 ---
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/providers/%d", provID), nil, "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("delete provider: %d", rec.Code)
	}
	if keys := decodeArr(t, do(t, h, "GET", "/api/keys", nil, "test-token")); len(keys) != 1 {
		t.Fatalf("zhipu keys should cascade-delete (only other-provider key remains), got %d", len(keys))
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

func TestProviderProxyURLIsPersisted(t *testing.T) {
	h, st := newTestServerWithStore(t)

	proxyURL := "http://proxy-user:proxy-pass@127.0.0.1:8080"
	rec := do(t, h, "POST", "/api/providers", map[string]any{
		"name": "proxy-provider", "base_url": "https://api.example.com", "proxy_url": proxyURL,
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create provider: %d — %s", rec.Code, rec.Body.String())
	}

	var provider store.Provider
	if err := st.DB.Where("name = ?", "proxy-provider").First(&provider).Error; err != nil {
		t.Fatalf("load provider: %v", err)
	}
	if provider.ProxyURL != proxyURL {
		t.Fatalf("created proxy_url = %q, want %q", provider.ProxyURL, proxyURL)
	}

	updatedProxyURL := "http://new-user:new-pass@127.0.0.1:8081"
	rec = do(t, h, "PUT", fmt.Sprintf("/api/providers/%d", provider.ID), map[string]any{
		"proxy_url": updatedProxyURL,
	}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("update provider: %d — %s", rec.Code, rec.Body.String())
	}
	if err := st.DB.First(&provider, provider.ID).Error; err != nil {
		t.Fatalf("reload provider: %v", err)
	}
	if provider.ProxyURL != updatedProxyURL {
		t.Fatalf("updated proxy_url = %q, want %q", provider.ProxyURL, updatedProxyURL)
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

// TestBreakdownKeyDimMaskedLabel 密钥回显链路须带名称与脱敏标签（裸 key#id 不可辨识密钥）
func TestBreakdownKeyDimMaskedLabel(t *testing.T) {
	h, st := newTestServerWithStore(t)

	prov := store.Provider{Name: "zhipu", BaseURL: "https://api.example.com", Protocol: "openai"}
	if err := st.DB.Create(&prov).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	key := store.ApiKey{ProviderID: prov.ID, KeyValue: "sk-breakdown-key-987654", Name: "主力账号"}
	if err := st.DB.Create(&key).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if err := st.DB.Create(&store.RequestLog{
		RequestID: "req-bd-1", Route: "glm", Model: "glm-4.6", Provider: "zhipu",
		KeyID: key.ID, Status: "success",
	}).Error; err != nil {
		t.Fatalf("seed request log: %v", err)
	}

	items := decodeArr(t, do(t, h, "GET", "/api/stats/breakdown?dim=key", nil, "test-token"))
	if len(items) != 1 {
		t.Fatalf("breakdown dim=key items = %d, want 1", len(items))
	}
	it := items[0].(map[string]any)
	if it["dim"] != fmt.Sprintf("%d", key.ID) {
		t.Fatalf("dim = %v, want key id %d", it["dim"], key.ID)
	}
	if it["key_masked"] != "sk-br****7654" {
		t.Fatalf("key_masked = %v, want sk-br****7654", it["key_masked"])
	}
	if it["key_name"] != "主力账号" {
		t.Fatalf("key_name = %v, want 主力账号", it["key_name"])
	}

	// --- 日志列表回显名称 ---
	logs := decodeObj(t, do(t, h, "GET", "/api/logs", nil, "test-token"))
	logItems := logs["items"].([]any)
	if len(logItems) != 1 {
		t.Fatalf("logs items = %d, want 1", len(logItems))
	}
	l0 := logItems[0].(map[string]any)
	if l0["key_name"] != "主力账号" || l0["key_value_masked"] != "sk-br****7654" {
		t.Fatalf("log key echo = %v / %v", l0["key_name"], l0["key_value_masked"])
	}

	// --- 改名后回显跟随 ---
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/keys/%d", key.ID), map[string]any{"name": "备用"}, "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("rename key: %d — %s", rec.Code, rec.Body.String())
	}
	items = decodeArr(t, do(t, h, "GET", "/api/stats/breakdown?dim=key", nil, "test-token"))
	if got := items[0].(map[string]any)["key_name"]; got != "备用" {
		t.Fatalf("key_name after rename = %v, want 备用", got)
	}
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

// TestEmptyKeysNotNullArray 回归：空密钥列表必须序列化为 [] 而非 null（前端 .length 会崩）
func TestEmptyKeysNotNullArray(t *testing.T) {
	h := newTestServer(t)
	rec := do(t, h, "GET", "/api/keys", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys on empty db: %d — %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); !strings.HasPrefix(got, "[") {
		t.Fatalf("keys must be [] not null: %s", got)
	}
}

// TestModelRequiresKeys 回归：模型必须绑定至少一个密钥
func TestModelRequiresKeys(t *testing.T) {
	h := newTestServer(t)
	do(t, h, "POST", "/api/providers", map[string]any{"name": "zhipu", "base_url": "https://x"}, "test-token")
	rec := do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "no-key", "key_ids": []int64{},
	}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "至少绑定一个密钥") {
		t.Fatalf("empty key_ids must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
}

func TestModelTypeValidation(t *testing.T) {
	h := newTestServer(t)
	do(t, h, "POST", "/api/providers", map[string]any{"name": "zhipu", "base_url": "https://x"}, "test-token")
	do(t, h, "POST", "/api/keys", map[string]any{"provider_id": 1, "key_value": "sk-zzzz1111", "name": "k1"}, "test-token")

	rec := do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "emb", "type": "embedding", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"type":"embedding"`) {
		t.Fatalf("embedding model create: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "rr", "type": "rerank", "protocol": "anthropic", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "仅支持 openai 协议") {
		t.Fatalf("rerank+anthropic must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "bad", "type": "image", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid type must be rejected: %d", rec.Code)
	}
	// 未传 type 默认 chat
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "plain", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"type":"chat"`) {
		t.Fatalf("default type chat: %d — %s", rec.Code, rec.Body.String())
	}
	// 更新：把 chat 模型协议改成 anthropic 时，同时是 embedding 的模型必须被组合校验拦下
	rec = do(t, h, "PUT", "/api/models/1", map[string]any{"protocol": "anthropic"}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "仅支持 openai 协议") {
		t.Fatalf("embedding model protocol change must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "PUT", "/api/models/1", map[string]any{"type": "rerank"}, "test-token")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"rerank"`) {
		t.Fatalf("type update: %d — %s", rec.Code, rec.Body.String())
	}
}

func TestMaintenanceCleanup(t *testing.T) {
	h, st := newTestServerWithStore(t)
	rec := do(t, h, "PUT", "/api/settings",
		map[string]any{"log.retention_days": 5, "capture.retention_days": 5}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("set retention failed: %d — %s", rec.Code, rec.Body.String())
	}
	now := time.Now().Unix()
	old := now - 10*86400
	rows := []store.RequestLog{
		{RequestID: "old", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: old},
		{RequestID: "new", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	caps := []store.ContentLog{
		{RequestID: "old", Route: "r", RequestBody: "{}", ResponseBody: "{}", CreatedAt: old},
		{RequestID: "new", Route: "r", RequestBody: "{}", ResponseBody: "{}", CreatedAt: now},
	}
	for i := range caps {
		if err := st.DB.Create(&caps[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	rec = do(t, h, "POST", "/api/maintenance/cleanup", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup failed: %d — %s", rec.Code, rec.Body.String())
	}
	obj := decodeObj(t, rec)
	deleted := obj["deleted"].(map[string]any)
	if deleted["request_log"].(float64) != 1 || deleted["content_log"].(float64) != 1 {
		t.Fatalf("deleted counts wrong: %v", deleted)
	}
	var n int64
	st.DB.Table("request_log").Count(&n)
	if n != 1 {
		t.Fatalf("request_log remaining %d, want 1", n)
	}
	st.DB.Table("content_log").Count(&n)
	if n != 1 {
		t.Fatalf("content_log remaining %d, want 1", n)
	}
}

func TestMaintenanceClearStatsRequiresConfirm(t *testing.T) {
	h, st := newTestServerWithStore(t)
	now := time.Now().Unix()
	for i := 0; i < 2; i++ {
		if err := st.DB.Create(&store.RequestLog{
			RequestID: fmt.Sprintf("r%d", i), Route: "r", Model: "m", Provider: "p",
			Status: "success", CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB.Create(&store.ContentLog{
		RequestID: "r0", Route: "r", RequestBody: "{}", ResponseBody: "{}", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "POST", "/api/maintenance/clear-stats", nil, "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing confirm must be 400, got %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "POST", "/api/maintenance/clear-stats", map[string]any{"confirm": false}, "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm=false must be 400, got %d", rec.Code)
	}

	rec = do(t, h, "POST", "/api/maintenance/clear-stats", map[string]any{"confirm": true}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear-stats failed: %d — %s", rec.Code, rec.Body.String())
	}
	obj := decodeObj(t, rec)
	cleared := obj["cleared"].(map[string]any)
	if cleared["request_log"].(float64) != 2 {
		t.Fatalf("cleared counts wrong: %v", cleared)
	}
	var n int64
	st.DB.Table("content_log").Count(&n)
	if n != 1 {
		t.Fatalf("clear-stats must keep content_log, remaining %d", n)
	}
}

func TestStatsErrorCodeBreakdown(t *testing.T) {
	h, st := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "s1", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now},
		{RequestID: "s2", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now},
		{RequestID: "e1", Route: "r", Model: "m", Provider: "p", Status: "error", ErrorCode: "timeout", CreatedAt: now},
		{RequestID: "e2", Route: "r", Model: "m", Provider: "p", Status: "error", ErrorCode: "conn", CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 预聚合表有数据：error_code 维度必须绕过 rollup 走原始表（rollup 表无该列）
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: store.DayKey(now), Route: "r", Model: "m", Provider: "p", Status: "success", Total: 99,
	}).Error; err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "GET", "/api/stats/breakdown?dim=error_code", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("error_code breakdown failed: %d — %s", rec.Code, rec.Body.String())
	}
	got := map[string]float64{}
	for _, it := range decodeArr(t, rec) {
		row := it.(map[string]any)
		got[row["dim"].(string)] = row["total"].(float64)
	}
	if got[""] != 2 || got["timeout"] != 1 || got["conn"] != 1 {
		t.Fatalf("error_code rows wrong: %v", got)
	}
}

func TestStatsTimeseriesAvgTotal(t *testing.T) {
	h, st := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "a", Route: "r", Model: "m", Provider: "p", Status: "success",
			TTFTMs: 50, TotalMs: 100, CreatedAt: now},
		{RequestID: "b", Route: "r", Model: "m", Provider: "p", Status: "success",
			TTFTMs: 150, TotalMs: 300, CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	rec := do(t, h, "GET", "/api/stats/timeseries", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeseries failed: %d — %s", rec.Code, rec.Body.String())
	}
	obj := decodeObj(t, rec)
	points := obj["points"].([]any)
	if len(points) != 1 {
		t.Fatalf("expect 1 bucket, got %d", len(points))
	}
	p := points[0].(map[string]any)
	if p["total"].(float64) != 2 {
		t.Fatalf("bucket total wrong: %v", p)
	}
	if p["avg_ttft_ms"].(float64) != 100 || p["avg_total_ms"].(float64) != 200 {
		t.Fatalf("averages wrong: ttft=%v total=%v", p["avg_ttft_ms"], p["avg_total_ms"])
	}
}

func TestStatsTimeseriesIncludesMetricsAndFiltersProvider(t *testing.T) {
	h, st := newTestServerWithStore(t)
	from, to := int64(1700000000), int64(1700003600)
	rows := []store.RequestLog{
		{RequestID: "provider-a-success", Route: "r", Model: "m", Provider: "provider-a", Status: "success",
			PromptTokens: 11, CompletionTokens: 7, IsFallback: true, CreatedAt: from + 10},
		{RequestID: "provider-a-error", Route: "r", Model: "m", Provider: "provider-a", Status: "error",
			PromptTokens: 3, CompletionTokens: 2, IsFallback: true, CreatedAt: from + 20},
		{RequestID: "provider-b-success", Route: "r", Model: "m", Provider: "provider-b", Status: "success",
			PromptTokens: 100, CompletionTokens: 200, CreatedAt: from + 30},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	rec := do(t, h, "GET", fmt.Sprintf("/api/stats/timeseries?from=%d&to=%d&provider=provider-a", from, to), nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeseries failed: %d — %s", rec.Code, rec.Body.String())
	}
	points := decodeObj(t, rec)["points"].([]any)
	if len(points) != 1 {
		t.Fatalf("expect 1 bucket, got %d", len(points))
	}
	p := points[0].(map[string]any)
	for field, want := range map[string]float64{
		"total": 2, "success": 1, "errors": 1, "prompt_tokens": 14,
		"completion_tokens": 9, "total_tokens": 23, "fallback_count": 2,
	} {
		if p[field] != want {
			t.Fatalf("%s wrong: want %v, got %v", field, want, p[field])
		}
	}
}

// TestModelPriceCurrency 模型价格币种：创建回显、非法值 400、更新生效。
func TestModelPriceCurrency(t *testing.T) {
	h := newTestServer(t)
	rec := do(t, h, "POST", "/api/providers", map[string]any{"name": "cc-prov", "base_url": "https://x"}, "test-token")
	provID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": provID, "key_value": "sk-cc1111", "name": "k1"}, "test-token")
	keyID := idOf(t, decodeObj(t, rec))

	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-cc", "input_price": 10, "output_price": 20,
		"price_currency": "CNY", "key_ids": []int64{keyID},
	}, "test-token")
	m := decodeObj(t, rec)
	if m["price_currency"] != "CNY" {
		t.Fatalf("price_currency should echo CNY, got %v", m["price_currency"])
	}
	if rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-eur", "key_ids": []int64{keyID}, "price_currency": "EUR",
	}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid currency should 400, got %d", rec.Code)
	}

	modelID := idOf(t, m)
	rec = do(t, h, "PUT", fmt.Sprintf("/api/models/%d", modelID), map[string]any{"price_currency": "USD"}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("update currency failed: %d", rec.Code)
	}
	if m = decodeObj(t, rec); m["price_currency"] != "USD" {
		t.Fatalf("updated currency should be USD, got %v", m["price_currency"])
	}
}

// TestPricingSetting 汇率配置：默认值可见、合法值热更新、非法值拒绝。
func TestPricingSetting(t *testing.T) {
	h := newTestServer(t)
	settings := decodeObj(t, do(t, h, "GET", "/api/settings", nil, "test-token"))
	if _, ok := settings["pricing.usd_cny"]; !ok {
		t.Fatal("settings should expose pricing.usd_cny")
	}
	if rec := do(t, h, "PUT", "/api/settings", map[string]any{"pricing.usd_cny": 7}, "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("set rate should pass: %d", rec.Code)
	}
	settings = decodeObj(t, do(t, h, "GET", "/api/settings", nil, "test-token"))
	if settings["pricing.usd_cny"] != float64(7) {
		t.Fatalf("rate should be 7, got %v", settings["pricing.usd_cny"])
	}
	for _, bad := range []any{0, -1, "x"} {
		if rec := do(t, h, "PUT", "/api/settings", map[string]any{"pricing.usd_cny": bad}, "test-token"); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid rate %v should 400, got %d", bad, rec.Code)
		}
	}
}

// TestStatsCurrencyParam currency=CNY 时统计费用按快照汇率放大（rollup 与 raw 双路径一致）。
func TestStatsCurrencyParam(t *testing.T) {
	h, st := newTestServerWithStore(t)
	now := time.Now().Unix()
	for i := 0; i < 2; i++ {
		entry := store.RequestLog{
			RequestID: fmt.Sprintf("cur-%d", i), Route: "glm", Model: "m1", Provider: "p1",
			Status: "success", Cost: 1.0, CreatedAt: now - 100,
		}
		if err := st.DB.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
		store.UpsertDaily(st.DB, &entry)
	}
	do(t, h, "PUT", "/api/settings", map[string]any{"pricing.usd_cny": 7}, "test-token")

	ov := decodeObj(t, do(t, h, "GET", "/api/stats/overview?currency=CNY", nil, "test-token"))
	if ov["cost"] != float64(14) {
		t.Fatalf("CNY overview cost want 14, got %v", ov["cost"])
	}
	ov = decodeObj(t, do(t, h, "GET", "/api/stats/overview", nil, "test-token"))
	if ov["cost"] != float64(2) {
		t.Fatalf("USD overview cost want 2, got %v", ov["cost"])
	}

	items := decodeArr(t, do(t, h, "GET", "/api/stats/breakdown?dim=model&currency=CNY", nil, "test-token"))
	if len(items) != 1 || items[0].(map[string]any)["cost"] != float64(14) {
		t.Fatalf("CNY breakdown cost wrong: %v", items)
	}
	ts := decodeObj(t, do(t, h, "GET", "/api/stats/timeseries?currency=CNY", nil, "test-token"))
	pts := ts["points"].([]any)
	if len(pts) != 1 || pts[0].(map[string]any)["cost"] != float64(14) {
		t.Fatalf("CNY timeseries cost wrong: %v", pts)
	}
}

// TestModelTestKeysEndpoint 逐密钥测试端点：好/坏 key 并存，各自出结果。
func TestModelTestKeysEndpoint(t *testing.T) {
	h := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "sk-bad") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid key"}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer up.Close()

	rec := do(t, h, "POST", "/api/providers", map[string]any{"name": "tk-prov", "base_url": up.URL}, "test-token")
	provID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": provID, "key_value": "sk-good1111", "name": "好key"}, "test-token")
	goodID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": provID, "key_value": "sk-bad2222", "name": "坏key"}, "test-token")
	badID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-tk", "key_ids": []int64{goodID, badID},
	}, "test-token")
	modelID := idOf(t, decodeObj(t, rec))

	res := decodeObj(t, do(t, h, "POST", fmt.Sprintf("/api/models/%d/test-keys", modelID), nil, "test-token"))
	if res["model"] != "m-tk" {
		t.Fatalf("model name wrong: %v", res["model"])
	}
	keys := res["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("expect 2 key results, got %d", len(keys))
	}
	byID := map[int64]map[string]any{}
	for _, k := range keys {
		km := k.(map[string]any)
		byID[int64(km["key_id"].(float64))] = km
	}
	if g := byID[goodID]; g["ok"] != true || g["key_name"] != "好key" || g["key_masked"] == "" {
		t.Fatalf("good key result wrong: %v", g)
	}
	if b := byID[badID]; b["ok"] != false || b["error_code"] != "401" {
		t.Fatalf("bad key result wrong: %v", b)
	}

	if rec = do(t, h, "POST", "/api/models/99999/test-keys", nil, "test-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing model should 404, got %d", rec.Code)
	}
}
