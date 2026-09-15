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

func newTestServerWithStore(t *testing.T) (http.Handler, *store.Store, string) {
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

	// 创建测试虚拟 key
	vk := &store.VirtualKey{
		Name:           "test-vk",
		Status:         "active",
		RPMLimit:       0,
		TotalBudgetUSD: 0,
		AllowedRoutes:  "[]",
	}
	if err := st.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}

	return New(st, rt, AdminAuth{Username: "admin", Password: "test-token"}, nil, nil).Router(), st, vk.KeyValue
}

func newTestServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	h, _, vk := newTestServerWithStore(t)
	return h, vk
}

// todayQuery 对齐本地自然日 00:00:00..23:59:59，满足 rollupCoversRange，强制走日聚合。
func todayQuery() string {
	now := time.Now().Unix()
	from := store.DayStartUnix(store.DayKey(now))
	to := store.NextDayStartUnix(store.DayKey(now)) - 1
	return fmt.Sprintf("from=%d&to=%d", from, to)
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

func doV1(t *testing.T, h http.Handler, method, path string, body any, vkToken string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if vkToken != "" {
		req.Header.Set("Authorization", "Bearer "+vkToken)
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
	h, _, vkToken := newTestServerWithStore(t)

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

	// --- /v1/models 返回逻辑路由名 + provider/model 直达 id ---
	v1 := decodeObj(t, doV1(t, h, "GET", "/v1/models", nil, vkToken))
	v1data := v1["data"].([]any)
	v1ids := map[string]bool{}
	for _, item := range v1data {
		v1ids[item.(map[string]any)["id"].(string)] = true
	}
	if !v1ids["glm-pool"] {
		t.Fatalf("v1/models missing logical route: %v", v1)
	}
	if !v1ids["zhipu/glm-4.6"] || !v1ids["zhipu/glm-4.5-flash"] {
		t.Fatalf("v1/models missing provider/model ids: %v", v1)
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
	if hm["status"] != "disabled" {
		t.Fatalf("model disable failed, status should be disabled: %v", hm)
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

func TestV1ModelsIncludesProviderModelIDs(t *testing.T) {
	h, st, vkToken := newTestServerWithStore(t)
	p := store.Provider{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1"}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m := store.Model{ProviderID: p.ID, Name: "anthropic/claude-3.5-sonnet"}
	if err := st.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	rt := store.Route{Name: "glm-pool"}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}

	v1 := decodeObj(t, doV1(t, h, "GET", "/v1/models", nil, vkToken))
	data, _ := v1["data"].([]any)
	ids := map[string]bool{}
	for _, item := range data {
		row, _ := item.(map[string]any)
		id, _ := row["id"].(string)
		ids[id] = true
		if row["owned_by"] != "omnigate" {
			t.Fatalf("owned_by = %v", row["owned_by"])
		}
	}
	if !ids["glm-pool"] {
		t.Fatalf("missing logical route: %v", ids)
	}
	if !ids["openrouter/anthropic/claude-3.5-sonnet"] {
		t.Fatalf("missing provider/model id: %v", ids)
	}
}

func TestLogRoutesIncludesProviderModel(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	p := store.Provider{Name: "SeekAI", BaseURL: "https://example.invalid"}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m := store.Model{ProviderID: p.ID, Name: "deepseek-v4", Protocol: "completions"}
	if err := st.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	rt := store.Route{Name: "glm-pool", Endpoint: "completions"}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}

	items := decodeArr(t, do(t, h, "GET", "/api/logs/routes", nil, "test-token"))
	got := map[string]string{}
	for _, it := range items {
		row := it.(map[string]any)
		got[row["name"].(string)] = row["kind"].(string)
	}
	if got["glm-pool"] != "route" {
		t.Fatalf("logical route missing: %v", got)
	}
	if got["SeekAI/deepseek-v4"] != "direct" {
		t.Fatalf("provider/model missing: %v", got)
	}

	filtered := decodeObj(t, do(t, h, "GET", "/api/logs?route=SeekAI/deepseek-v4", nil, "test-token"))
	if filtered["total"].(float64) != 0 {
		t.Fatalf("empty filter should be 0, got %v", filtered["total"])
	}
}

func TestProviderProxyURLIsPersisted(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)

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
	h, st, _ := newTestServerWithStore(t)

	prov := store.Provider{Name: "zhipu", BaseURL: "https://api.example.com", Protocol: "completions"}
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
	h, _ := newTestServer(t)
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
	h, _ := newTestServer(t)
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
	h, _ := newTestServer(t)
	do(t, h, "POST", "/api/providers", map[string]any{"name": "zhipu", "base_url": "https://x"}, "test-token")
	rec := do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "no-key", "key_ids": []int64{},
	}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "至少绑定一个密钥") {
		t.Fatalf("empty key_ids must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
}

func TestModelTypeValidation(t *testing.T) {
	h, _ := newTestServer(t)
	do(t, h, "POST", "/api/providers", map[string]any{"name": "zhipu", "base_url": "https://x"}, "test-token")
	do(t, h, "POST", "/api/keys", map[string]any{"provider_id": 1, "key_value": "sk-zzzz1111", "name": "k1"}, "test-token")

	rec := do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "emb", "type": "embedding", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"type":"embedding"`) {
		t.Fatalf("embedding model create: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "rr", "type": "rerank", "protocol": "messages", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "仅支持 completions 协议") {
		t.Fatalf("rerank+messages must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "bad", "type": "hologram", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid type must be rejected: %d", rec.Code)
	}
	// video 为合法类型（异步生成端点），且 protocol 仍锁 completions
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "vid", "type": "video", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"type":"video"`) {
		t.Fatalf("video model create: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "vid2", "type": "video", "protocol": "messages", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "仅支持 completions 协议") {
		t.Fatalf("video+messages must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
	// 未传 type 默认 chat
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "plain", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"type":"chat"`) {
		t.Fatalf("default type chat: %d — %s", rec.Code, rec.Body.String())
	}
	// 更新：把 chat 模型协议改成 messages 时，同时是 embedding 的模型必须被组合校验拦下
	rec = do(t, h, "PUT", "/api/models/1", map[string]any{"protocol": "messages"}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "仅支持 completions 协议") {
		t.Fatalf("embedding model protocol change must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "PUT", "/api/models/1", map[string]any{"type": "rerank"}, "test-token")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"rerank"`) {
		t.Fatalf("type update: %d — %s", rec.Code, rec.Body.String())
	}
	// image 模型：可创建，但 protocol 必须 completions
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "img", "type": "image", "protocol": "messages", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "仅支持 completions 协议") {
		t.Fatalf("image+messages must be rejected: %d — %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": 1, "name": "img", "type": "image", "key_ids": []int64{1},
	}, "test-token")
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"type":"image"`) {
		t.Fatalf("image model create: %d — %s", rec.Code, rec.Body.String())
	}
}

func TestMaintenanceCleanup(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
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
	h, st, _ := newTestServerWithStore(t)
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

func TestMaintenanceClearLogs(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	for i := range 2 {
		if err := st.DB.Create(&store.RequestLog{
			RequestID: fmt.Sprintf("r%d", i), Route: "r", Model: "m", Provider: "p",
			Status: "success", CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB.Create(&store.RequestAttempt{
		RequestID: "r0", Attempt: 1, Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: store.DayKey(now), Route: "r", Model: "m", Provider: "p", Status: "success", Total: 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ContentLog{
		RequestID: "r0", Route: "r", RequestBody: "{}", ResponseBody: "{}", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.VideoTask{
		VideoID: "vid-1", Route: "r", ModelID: 1, ProviderID: 1, KeyID: 1, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "POST", "/api/maintenance/clear-logs", nil, "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing confirm must be 400, got %d — %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, "POST", "/api/maintenance/clear-logs", map[string]any{"confirm": true}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear-logs failed: %d — %s", rec.Code, rec.Body.String())
	}
	obj := decodeObj(t, rec)
	cleared := obj["cleared"].(map[string]any)
	if cleared["request_log"].(float64) != 2 || cleared["request_attempt"].(float64) != 1 ||
		cleared["content_log"].(float64) != 1 || cleared["video_task"].(float64) != 1 {
		t.Fatalf("cleared counts wrong: %v", cleared)
	}
	var n int64
	st.DB.Table("request_log_daily").Count(&n)
	if n != 1 {
		t.Fatalf("clear-logs must keep request_log_daily, remaining %d", n)
	}
	var cl int64
	st.DB.Table("content_log").Count(&cl)
	if cl != 0 {
		t.Fatalf("clear-logs must clear content_log, remaining %d", cl)
	}
	var vt int64
	st.DB.Table("video_task").Count(&vt)
	if vt != 0 {
		t.Fatalf("clear-logs must clear video_task, remaining %d", vt)
	}
}

func TestStatsErrorCodeBreakdown(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "s1", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now},
		{RequestID: "s2", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now},
		{RequestID: "e1", Route: "r", Model: "m", Provider: "p", Status: "error", ErrorCode: "timeout", CreatedAt: now},
		{RequestID: "e2", Route: "r", Model: "m", Provider: "p", Status: "error", ErrorCode: "connection_failed", CreatedAt: now},
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
	if got[""] != 2 || got["timeout"] != 1 || got["connection_failed"] != 1 {
		t.Fatalf("error_code rows wrong: %v", got)
	}
}

// TestLogsProviderFilterAndModelBreakdown 回归两处契约：
//  1. /api/logs 支持 provider 过滤（前端日志页提供商筛选项依赖它）；
//  2. dim=model 的分布按 (provider, model) 分组并以 provider/model 呈现，
//     跨提供商的同名模型不得被合并成一行。
func TestLogsProviderFilterAndModelBreakdown(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "p1-1", Route: "glm", Model: "glm-5", Provider: "Zhipu", Status: "success", CreatedAt: now},
		{RequestID: "p1-2", Route: "glm", Model: "glm-5", Provider: "Zhipu", Status: "success", CreatedAt: now},
		{RequestID: "p2-1", Route: "glm", Model: "glm-5", Provider: "Bailian", Status: "success", CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	filtered := decodeObj(t, do(t, h, "GET", "/api/logs?provider=Zhipu", nil, "test-token"))
	if filtered["total"].(float64) != 2 {
		t.Fatalf("provider filter total = %v, want 2", filtered["total"])
	}
	for _, it := range filtered["items"].([]any) {
		if got := it.(map[string]any)["provider"]; got != "Zhipu" {
			t.Fatalf("filtered row provider = %v, want Zhipu", got)
		}
	}
	if all := decodeObj(t, do(t, h, "GET", "/api/logs", nil, "test-token")); all["total"].(float64) != 3 {
		t.Fatalf("unfiltered total = %v, want 3", all["total"])
	}

	got := map[string]float64{}
	for _, it := range decodeArr(t, do(t, h, "GET", "/api/stats/breakdown?dim=model", nil, "test-token")) {
		row := it.(map[string]any)
		got[row["dim"].(string)] = row["total"].(float64)
	}
	if len(got) != 2 || got["Zhipu/glm-5"] != 2 || got["Bailian/glm-5"] != 1 {
		t.Fatalf("model breakdown rows wrong: %v", got)
	}
}

func TestStatsTimeseriesAvgTotal(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
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
	h, st, _ := newTestServerWithStore(t)
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

// TestModelPriceCurrency 模型价格：缓存价回显与非法值、币种回显与非法值、更新生效。
func TestModelPriceCurrency(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "POST", "/api/providers", map[string]any{"name": "cc-prov", "base_url": "https://x"}, "test-token")
	provID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": provID, "key_value": "sk-cc1111", "name": "k1"}, "test-token")
	keyID := idOf(t, decodeObj(t, rec))

	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-cc", "input_price": 10, "cached_price": 5, "output_price": 20,
		"price_currency": "CNY", "key_ids": []int64{keyID},
	}, "test-token")
	m := decodeObj(t, rec)
	if m["price_currency"] != "CNY" {
		t.Fatalf("price_currency should echo CNY, got %v", m["price_currency"])
	}
	if m["cached_price"] != float64(5) {
		t.Fatalf("cached_price should echo 5, got %v", m["cached_price"])
	}
	if m["billing_mode"] != "token" {
		t.Fatalf("billing_mode should default to token, got %v", m["billing_mode"])
	}
	if rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-neg", "cached_price": -1, "key_ids": []int64{keyID},
	}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative cached_price should 400, got %d", rec.Code)
	}
	if rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-eur", "key_ids": []int64{keyID}, "price_currency": "EUR",
	}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid currency should 400, got %d", rec.Code)
	}
	if rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-bad-mode", "key_ids": []int64{keyID}, "billing_mode": "hourly",
	}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid billing_mode should 400, got %d", rec.Code)
	}
	if rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-neg-call", "key_ids": []int64{keyID}, "per_call_price": -1,
	}, "test-token"); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative per_call_price should 400, got %d", rec.Code)
	}

	modelID := idOf(t, m)
	rec = do(t, h, "PUT", fmt.Sprintf("/api/models/%d", modelID), map[string]any{
		"price_currency": "USD", "cached_price": 3,
	}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("update price fields failed: %d", rec.Code)
	}
	if m = decodeObj(t, rec); m["price_currency"] != "USD" || m["cached_price"] != float64(3) {
		t.Fatalf("updated price fields wrong: currency=%v cached=%v", m["price_currency"], m["cached_price"])
	}

	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "m-call", "billing_mode": "per_call", "per_call_price": 0.02,
		"price_currency": "USD", "key_ids": []int64{keyID},
	}, "test-token")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create per_call model failed: %d — %s", rec.Code, rec.Body.String())
	}
	m = decodeObj(t, rec)
	if m["billing_mode"] != "per_call" || m["per_call_price"] != 0.02 {
		t.Fatalf("per_call fields wrong: mode=%v price=%v", m["billing_mode"], m["per_call_price"])
	}
	rec = do(t, h, "PUT", fmt.Sprintf("/api/models/%d", idOf(t, m)), map[string]any{
		"billing_mode": "token", "per_call_price": 0.05,
	}, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("update billing_mode failed: %d", rec.Code)
	}
	if m = decodeObj(t, rec); m["billing_mode"] != "token" || m["per_call_price"] != 0.05 {
		t.Fatalf("updated billing fields wrong: mode=%v price=%v", m["billing_mode"], m["per_call_price"])
	}
}

// TestPricingSetting 汇率配置：默认值可见、合法值热更新、非法值拒绝。
func TestPricingSetting(t *testing.T) {
	h, _ := newTestServer(t)
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
	h, st, _ := newTestServerWithStore(t)
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

// TestStatsOverviewRollupLatency 回归：预聚合路径的 p95 必须自低桶累加定位（不是自顶 5%）、
// 并在命中的桶内线性插值（不是直接返回桶上界）；延迟直方图只取成功请求——错误行
// ttft/total 为 0，混入会把计数堆进 0 号桶。
func TestStatsOverviewRollupLatency(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	day := store.DayKey(now)
	// 100 次成功：90 次 ttft/total 落在 6 号桶，10 次落在 8 号桶，p95 秩 95 落在 8 号桶中部
	// → TTFT p95 = 10000+(95-90)/10*(30000-10000) = 20000ms（均值 (90*3500+10*20000)/100 = 5150ms）
	// → 总耗时 p95 = 120000+(95-90)/10*(300000-120000) = 210000ms（均值 (90*45000+10*210000)/100 = 61500ms）
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: day, Route: "glm", Model: "m1", Provider: "p1", Status: "success",
		Total: 100, Success: 100,
		TTFTB6: 90, TTFTB8: 10, TotalB6: 90, TotalB8: 10,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 100 次错误：延迟为 0，全部落在 0 号桶，不得进入延迟统计口径
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: day, Route: "glm", Model: "m1", Provider: "p1", Status: "error",
		Total: 100, Errors: 100,
		TTFTB0: 100, TotalB0: 100,
	}).Error; err != nil {
		t.Fatal(err)
	}

	ov := decodeObj(t, do(t, h, "GET", "/api/stats/overview?"+todayQuery(), nil, "test-token"))

	if ov["total"] != float64(200) || ov["success"] != float64(100) {
		t.Fatalf("totals must cover every status: %v", ov)
	}
	if ov["p95_ttft_ms"] != float64(20000) {
		t.Fatalf("p95_ttft_ms want 20000, got %v", ov["p95_ttft_ms"])
	}
	if ov["avg_ttft_ms"] != float64(5150) {
		t.Fatalf("avg_ttft_ms want 5150 (errors excluded), got %v", ov["avg_ttft_ms"])
	}
	if ov["p95_total_ms"] != float64(210000) {
		t.Fatalf("p95_total_ms want 210000, got %v", ov["p95_total_ms"])
	}
	if ov["avg_total_ms"] != float64(61500) {
		t.Fatalf("avg_total_ms want 61500 (errors excluded), got %v", ov["avg_total_ms"])
	}
}

// TestModelTestKeysEndpoint 逐密钥测试端点：好/坏 key 并存，各自出结果。
func TestModelTestKeysEndpoint(t *testing.T) {
	h, _ := newTestServer(t)
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

	// 组合禁用明细：初始全 active → 手动禁用单个 → 明细反映 perm_banned → 解禁后恢复
	bans := decodeArr(t, do(t, h, "GET", fmt.Sprintf("/api/models/%d/bans", modelID), nil, "test-token"))
	if len(bans) != 2 {
		t.Fatalf("expect 2 ban rows, got %d", len(bans))
	}
	for _, b := range bans {
		if b.(map[string]any)["status"] != "active" {
			t.Fatalf("expect all active initially: %v", b)
		}
	}
	if rec = do(t, h, "POST", fmt.Sprintf("/api/models/%d/bans/%d", modelID, badID), nil, "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("ban combo: %d — %s", rec.Code, rec.Body.String())
	}
	bans = decodeArr(t, do(t, h, "GET", fmt.Sprintf("/api/models/%d/bans", modelID), nil, "test-token"))
	byID = map[int64]map[string]any{}
	for _, b := range bans {
		bm := b.(map[string]any)
		byID[int64(bm["key_id"].(float64))] = bm
	}
	if byID[badID]["status"] != "perm_banned" || byID[badID]["ban_reason"] == "" {
		t.Fatalf("banned combo detail wrong: %v", byID[badID])
	}
	if byID[goodID]["status"] != "active" {
		t.Fatalf("good combo should stay active: %v", byID[goodID])
	}
	if rec = do(t, h, "DELETE", fmt.Sprintf("/api/models/%d/bans/%d", modelID, badID), nil, "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("unban combo: %d — %s", rec.Code, rec.Body.String())
	}
	bans = decodeArr(t, do(t, h, "GET", fmt.Sprintf("/api/models/%d/bans", modelID), nil, "test-token"))
	for _, b := range bans {
		if b.(map[string]any)["status"] != "active" {
			t.Fatalf("expect active after unban: %v", b)
		}
	}
	if rec = do(t, h, "GET", "/api/models/99999/bans", nil, "test-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing model bans should 404, got %d", rec.Code)
	}
}

// TestModelTestKeysByNameEndpoint provider/model 直达测试：
// 模型名可含 /（仅第一个 / 作为分隔），提供商或模型缺失给 404，无 / 给 400。
func TestModelTestKeysByNameEndpoint(t *testing.T) {
	h, _ := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer up.Close()

	rec := do(t, h, "POST", "/api/providers", map[string]any{"name": "tn-prov", "base_url": up.URL}, "test-token")
	provID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/keys", map[string]any{"provider_id": provID, "key_value": "sk-tn-good", "name": "k1"}, "test-token")
	keyID := idOf(t, decodeObj(t, rec))
	rec = do(t, h, "POST", "/api/models", map[string]any{
		"provider_id": provID, "name": "dir/a/b", "key_ids": []int64{keyID},
	}, "test-token")
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create model: %d %s", rec.Code, rec.Body.String())
	}

	// 名字含多个 / ：按第一个 / 拆，匹配 dir/a/b
	res := decodeObj(t, do(t, h, "POST", "/api/models/test-by-name", map[string]any{"name": "tn-prov/dir/a/b"}, "test-token"))
	if res["model"] != "dir/a/b" || res["provider"] != "tn-prov" {
		t.Fatalf("test-by-name result wrong: %v", res)
	}
	if keys, ok := res["keys"].([]any); !ok || len(keys) != 1 {
		t.Fatalf("expect 1 key result, got %v", res["keys"])
	}

	// 提供商存在但模型不存在 → 404
	if rec = do(t, h, "POST", "/api/models/test-by-name", map[string]any{"name": "tn-prov/nope"}, "test-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing model should 404, got %d", rec.Code)
	}
	// 提供商不存在 → 404
	if rec = do(t, h, "POST", "/api/models/test-by-name", map[string]any{"name": "nope/m"}, "test-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing provider should 404, got %d", rec.Code)
	}
	// 无 / 或空 → 400
	for _, bad := range []string{"nope", "", "/m", "p/", "  "} {
		if rec = do(t, h, "POST", "/api/models/test-by-name", map[string]any{"name": bad}, "test-token"); rec.Code != http.StatusBadRequest {
			t.Fatalf("name %q should 400, got %d", bad, rec.Code)
		}
	}
}

func TestFirstLevelNamespaces(t *testing.T) {
	h, vk := newTestServer(t)

	t.Run("root redirects to manage", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("GET / code = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/manage/" {
			t.Fatalf("GET / Location = %q, want /manage/", loc)
		}
	})

	t.Run("manage serves html", func(t *testing.T) {
		rec := do(t, h, "GET", "/manage", nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /manage code = %d, want 200", rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Fatalf("GET /manage Content-Type = %q, want text/html", ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<html") && !strings.Contains(body, "<!DOCTYPE html>") {
			t.Fatalf("GET /manage body is not HTML")
		}
	})

	t.Run("manage spa fallback", func(t *testing.T) {
		rec := do(t, h, "GET", "/manage/dashboard", nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /manage/dashboard code = %d, want 200", rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Fatalf("GET /manage/dashboard Content-Type = %q, want text/html", ct)
		}
	})

	t.Run("old spa path is not at root", func(t *testing.T) {
		rec := do(t, h, "GET", "/dashboard", nil, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /dashboard code = %d, want 404", rec.Code)
		}
	})

	t.Run("api stays at first level", func(t *testing.T) {
		rec := do(t, h, "GET", "/api/health", nil, "test-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/health code = %d, want 200", rec.Code)
		}
	})

	t.Run("v1 stays at first level", func(t *testing.T) {
		rec := doV1(t, h, "GET", "/v1/models", nil, vk)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/models code = %d, want 200", rec.Code)
		}
	})
}

// TestStatsOverviewTPSRawPath 原始表回退路径：avg_tps 取 tps>0 行的均值，p95_tps 取分位（截断为整数毫秒口径）。
func TestStatsOverviewTPSRawPath(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "tps-a", Route: "r", Model: "m", Provider: "p", Status: "success",
			IsStream: true, CompletionTokens: 150, TTFTMs: 200, TotalMs: 1700, Tps: 100, CreatedAt: now},
		// 非流式：tps=0，不进统计
		{RequestID: "tps-c", Route: "r", Model: "m", Provider: "p", Status: "success",
			CompletionTokens: 50, TTFTMs: 900, TotalMs: 900, CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	rec := do(t, h, "GET", "/api/stats/overview", nil, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview failed: %d — %s", rec.Code, rec.Body.String())
	}
	ov := decodeObj(t, rec)
	if avg := ov["avg_tps"].(float64); avg < 99.9 || avg > 100.1 {
		t.Fatalf("avg_tps want 100, got %v", avg)
	}
	if p95 := ov["p95_tps"].(float64); p95 != 100 {
		t.Fatalf("p95_tps want 100, got %v", p95)
	}
}

// TestStatsTimeseriesAndBreakdownTPS 时间序列与维度聚合的 avg_tps：只对 tps>0 行求均值。
func TestStatsTimeseriesAndBreakdownTPS(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "ts-a", Route: "r1", Model: "m", Provider: "p", Status: "success",
			IsStream: true, CompletionTokens: 30, TTFTMs: 200, TotalMs: 1200, Tps: 30, CreatedAt: now},
		{RequestID: "ts-b", Route: "r1", Model: "m", Provider: "p", Status: "success",
			IsStream: true, CompletionTokens: 10, TTFTMs: 200, TotalMs: 1200, Tps: 10, CreatedAt: now},
		{RequestID: "ts-c", Route: "r2", Model: "m", Provider: "p", Status: "success",
			CompletionTokens: 10, TTFTMs: 300, TotalMs: 300, CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	ts := decodeObj(t, do(t, h, "GET", "/api/stats/timeseries", nil, "test-token"))
	points := ts["points"].([]any)
	if len(points) != 1 {
		t.Fatalf("expect 1 bucket, got %d", len(points))
	}
	if avg := points[0].(map[string]any)["avg_tps"].(float64); avg < 19.9 || avg > 20.1 {
		t.Fatalf("timeseries avg_tps want 20, got %v", avg)
	}

	items := decodeArr(t, do(t, h, "GET", "/api/stats/breakdown?dim=route", nil, "test-token"))
	got := map[string]float64{}
	for _, it := range items {
		row := it.(map[string]any)
		got[row["dim"].(string)] = row["avg_tps"].(float64)
	}
	if got["r1"] < 19.9 || got["r1"] > 20.1 {
		t.Fatalf("r1 avg_tps want 20, got %v", got["r1"])
	}
	if got["r2"] != 0 {
		t.Fatalf("r2 avg_tps want 0 (no samples), got %v", got["r2"])
	}
}

func TestStatsOverviewSubDayDoesNotUseRollup(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	yesterday := now - 86400
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: store.DayKey(yesterday), Route: "r", Model: "m", Provider: "p", Status: "success",
		Total: 50, Success: 50, PromptTokens: 999,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RequestLog{
		RequestID: "now", Route: "r", Model: "m", Provider: "p", Status: "success",
		PromptTokens: 3, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	from, to := now-3600, now
	ov := decodeObj(t, do(t, h, "GET", fmt.Sprintf("/api/stats/overview?from=%d&to=%d", from, to), nil, "test-token"))
	if ov["total"] != float64(1) {
		t.Fatalf("24h/sub-day window must not expand to full daily rollup, got total=%v", ov["total"])
	}
	if ov["prompt_tokens"] != float64(3) {
		t.Fatalf("prompt_tokens want 3, got %v", ov["prompt_tokens"])
	}
}

func TestRollupCoversRange(t *testing.T) {
	now := time.Now().Unix()
	today := store.DayKey(now)
	start := store.DayStartUnix(today)
	endExcl := store.NextDayStartUnix(today)
	prevStart := store.DayStartUnix(store.DayKey(start - 1))
	tests := []struct {
		name     string
		from, to int64
		want     bool
	}{
		{"today full day", start, endExcl - 1, true},
		{"today open-ended at next midnight", start, endExcl, false},
		{"two full days", prevStart, endExcl - 1, true},
		{"2h inside today", now - 7200, now, false},
		{"24h rolling", now - 86400, now, false},
		{"today from midnight until mid-day", start, start + 12*3600, false},
		{"inverted", endExcl, start, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollupCoversRange(tt.from, tt.to); got != tt.want {
				t.Fatalf("rollupCoversRange(%d,%d)=%v want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

// TestStatsOverviewSameDayPartialDoesNotUseRollup 复现生产仪表盘：当天日聚合已有数据，
// 「最近 2 小时」仍落在今天。旧逻辑只要 rollupHasData 就按整天 SUM，卡片吃进全天
// （今天 2 亿、2 小时 4 亿这种倒置），图表 timeseries 仍按小时明细所以对不上。
func TestStatsOverviewSameDayPartialDoesNotUseRollup(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	dayStart := store.DayStartUnix(store.DayKey(now))
	// 钉在当天 16:00，保证 2h 窗仍是子日窗，不依赖测试运行时刻。
	afternoon := dayStart + 16*3600
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: store.DayKey(now), Route: "r", Model: "m", Provider: "p", Status: "success",
		Total: 100, Success: 100, PromptTokens: 200_000_000, CompletionTokens: 1_000_000,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RequestLog{
		RequestID: "recent", Route: "r", Model: "m", Provider: "p", Status: "success",
		PromptTokens: 20_000_000, CompletionTokens: 100_000, CreatedAt: afternoon - 600,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RequestLog{
		RequestID: "morning", Route: "r", Model: "m", Provider: "p", Status: "success",
		PromptTokens: 180_000_000, CompletionTokens: 900_000, CreatedAt: dayStart + 60,
	}).Error; err != nil {
		t.Fatal(err)
	}

	from2h, to2h := afternoon-7200, afternoon
	ov := decodeObj(t, do(t, h, "GET", fmt.Sprintf("/api/stats/overview?from=%d&to=%d", from2h, to2h), nil, "test-token"))
	if ov["total"] != float64(1) || ov["total_tokens"] != float64(20_100_000) {
		t.Fatalf("2h window must use raw rows, got total=%v tokens=%v", ov["total"], ov["total_tokens"])
	}

	bd := decodeArr(t, do(t, h, "GET", fmt.Sprintf("/api/stats/breakdown?dim=model&from=%d&to=%d", from2h, to2h), nil, "test-token"))
	if len(bd) != 1 {
		t.Fatalf("2h breakdown want 1 row, got %v", bd)
	}
	row := bd[0].(map[string]any)
	if row["total"] != float64(1) || row["prompt_tokens"] != float64(20_000_000) {
		t.Fatalf("2h breakdown must not expand to full day: %v", row)
	}

	from24h := afternoon - 86400
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: store.DayKey(afternoon - 86400), Route: "r", Model: "m", Provider: "p", Status: "success",
		Total: 80, Success: 80, PromptTokens: 200_000_000, CompletionTokens: 2_000_000,
	}).Error; err != nil {
		t.Fatal(err)
	}
	ov24 := decodeObj(t, do(t, h, "GET", fmt.Sprintf("/api/stats/overview?from=%d&to=%d", from24h, afternoon), nil, "test-token"))
	if ov24["total"] != float64(2) || ov24["total_tokens"] != float64(201_000_000) {
		t.Fatalf("24h window must use raw rows, got total=%v tokens=%v", ov24["total"], ov24["total_tokens"])
	}
}

func TestStatsPendingAndClientErrorExcluded(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "ok", Route: "r", Model: "m", Provider: "p", Status: "success", CreatedAt: now},
		{RequestID: "err", Route: "r", Model: "m", Provider: "p", Status: "error", CreatedAt: now},
		{RequestID: "ce", Route: "r", Model: "m", Provider: "p", Status: "client_error", CreatedAt: now},
		{RequestID: "pend", Route: "r", Model: "m", Provider: "p", Status: "pending", CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	ov := decodeObj(t, do(t, h, "GET", "/api/stats/overview", nil, "test-token"))
	if ov["total"] != float64(3) {
		t.Fatalf("pending must be excluded from total, got %v", ov["total"])
	}
	if ov["errors"] != float64(1) {
		t.Fatalf("only status=error counts as errors, got %v", ov["errors"])
	}
}

func TestStatsOverviewHonorsRouteFilter(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "a", Route: "glm", Model: "m", Provider: "p", Status: "success", PromptTokens: 10, CreatedAt: now},
		{RequestID: "b", Route: "other", Model: "m", Provider: "p", Status: "success", PromptTokens: 99, CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	ov := decodeObj(t, do(t, h, "GET", "/api/stats/overview?route=glm", nil, "test-token"))
	if ov["total"] != float64(1) || ov["prompt_tokens"] != float64(10) {
		t.Fatalf("route filter wrong: %v", ov)
	}
}

func TestStatsOverviewFilterProviderModel(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "a", Route: "r", Model: "glm-5", Provider: "Zhipu", Status: "success", CreatedAt: now},
		{RequestID: "b", Route: "r", Model: "glm-5", Provider: "Bailian", Status: "success", CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	ov := decodeObj(t, do(t, h, "GET", "/api/stats/overview?model=Zhipu/glm-5", nil, "test-token"))
	if ov["total"] != float64(1) {
		t.Fatalf("provider/model filter wrong: %v", ov)
	}
}

func TestStatsFallbackUsesLocalDayWindow(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	day := store.DayKey(now)
	if err := st.DB.Create(&store.RequestLogDaily{
		Day: day, Route: "r", Model: "m", Provider: "p", Status: "success", Total: 2, Success: 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RequestLog{
		RequestID: "fb", Route: "r", Model: "m", Provider: "p", Status: "success",
		IsFallback: true, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	ov := decodeObj(t, do(t, h, "GET", "/api/stats/overview?"+todayQuery(), nil, "test-token"))

	if ov["fallback_count"] != float64(1) {
		t.Fatalf("fallback_count want 1, got %v", ov["fallback_count"])
	}
}

func TestLogsEndpointFamilyFilter(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	rows := []store.RequestLog{
		{RequestID: "c", Route: "r", Endpoint: "completions", Status: "success", CreatedAt: now},
		{RequestID: "m", Route: "r", Endpoint: "messages", Status: "success", CreatedAt: now},
		{RequestID: "e", Route: "r", Endpoint: "embedding", Status: "success", CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	got := decodeObj(t, do(t, h, "GET", "/api/logs?endpoint=chat", nil, "test-token"))
	if got["total"].(float64) != 2 {
		t.Fatalf("chat family should match completions+messages, got %v", got["total"])
	}
}

func TestGetVKStatsSQLAndTimeRange(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	now := time.Now().Unix()
	vk := store.VirtualKey{Name: "vk-a", Status: "active"}
	if err := st.CreateVirtualKey(&vk); err != nil {
		t.Fatal(err)
	}
	rows := []store.RequestLog{
		{RequestID: "ok", Route: "r", Status: "success", VKID: vk.ID, Cost: 0.5, CreatedAt: now},
		{RequestID: "err", Route: "r", Status: "error", VKID: vk.ID, Cost: 0.1, CreatedAt: now},
		{RequestID: "pend", Route: "r", Status: "pending", VKID: vk.ID, Cost: 9, CreatedAt: now},
		{RequestID: "old", Route: "r", Status: "success", VKID: vk.ID, Cost: 8, CreatedAt: now - 86400*3},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	items := decodeArr(t, do(t, h, "GET", fmt.Sprintf("/api/stats/vk?from=%d&to=%d", now-60, now+60), nil, "test-token"))
	if len(items) != 1 {
		t.Fatalf("expect 1 vk row, got %v", items)
	}
	row := items[0].(map[string]any)
	if row["requests"] != float64(2) || row["success_count"] != float64(1) || row["error_count"] != float64(1) {
		t.Fatalf("vk stats wrong: %v", row)
	}
	if row["total_cost"] != 0.6 {
		t.Fatalf("vk cost want 0.6, got %v", row["total_cost"])
	}
}
