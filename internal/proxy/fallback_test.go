package proxy_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// seedFallbackFixture 构造「主后端全灭 + 可用兜底模型」的 fixture：
// 路由 glm-pool 的目标模型 m0 密钥组合被 perm_banned（首跳即无候选），
// 兜底模型 m1 指向 up 返回 200。返回路由（已设置 FallbackModelID）。
func seedFallbackFixture(t *testing.T, st *store.Store, upURL string, fallbackEnabled bool) *store.Route {
	t.Helper()
	p := store.Provider{Name: "zhipu", BaseURL: upURL}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m0 := store.Model{ProviderID: p.ID, Name: "m0"}
	if err := st.DB.Create(&m0).Error; err != nil {
		t.Fatal(err)
	}
	k0 := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-dead", Status: "active"}
	if err := st.DB.Create(&k0).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKey{ModelID: m0.ID, KeyID: k0.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKeyBan{ModelID: m0.ID, KeyID: k0.ID, Status: "perm_banned", BanReason: "连续3次超时"}).Error; err != nil {
		t.Fatal(err)
	}

	rt := store.Route{Name: "glm-pool"}
	if fallbackEnabled {
		m1 := store.Model{ProviderID: p.ID, Name: "m1"}
		if err := st.DB.Create(&m1).Error; err != nil {
			t.Fatal(err)
		}
		k1 := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-alive", Status: "active"}
		if err := st.DB.Create(&k1).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.DB.Create(&store.ModelKey{ModelID: m1.ID, KeyID: k1.ID}).Error; err != nil {
			t.Fatal(err)
		}
		rt.FallbackModelID = m1.ID
	}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m0.ID, Weight: 1}).Error; err != nil {
		t.Fatal(err)
	}
	return &rt
}

// TestRouteFallbackHit 路由配置了兜底模型：主后端全灭时兜底接管，记 is_fallback=1。
func TestRouteFallbackHit(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m1","choices":[{"message":{"role":"assistant","content":"from fallback"}}]}`)
	}))
	defer up.Close()
	seedFallbackFixture(t, st, up.URL, true)

	resp := postWithAuth(t, h, chatBody(false), vkToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expect 200 from fallback model, got %d: %s", resp.StatusCode, b)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["model"] != "m1" {
		t.Fatalf("response model = %v, want m1 (fallback model)", body["model"])
	}

	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("request_log rows = %d, want 1", len(ls))
	}
	if ls[0].Model != "m1" || ls[0].Status != "success" || !ls[0].IsFallback {
		t.Fatalf("log wrong: model=%s status=%s is_fallback=%v", ls[0].Model, ls[0].Status, ls[0].IsFallback)
	}
}

// TestRouteFallbackDisabled 路由未配置兜底（fallback_model_id=0）：主后端全灭直接 503。
func TestRouteFallbackDisabled(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("fallback model must not be called when route has none configured")
	}))
	defer up.Close()
	seedFallbackFixture(t, st, up.URL, false)

	resp := postWithAuth(t, h, chatBody(false), vkToken)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expect 503, got %d", resp.StatusCode)
	}
	ls := logs(t, st)
	if len(ls) != 1 || ls[0].ErrorCode != "all_backends_unavailable" || ls[0].IsFallback {
		t.Fatalf("log wrong: %+v", ls[0])
	}
}

// TestRouteFallbackHotUpdate 兜底配置改动即时生效：先 503，配置兜底后同一路由命中兜底。
func TestRouteFallbackHotUpdate(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer up.Close()
	// 兜底模型始终存在，但路由先不指向它（模拟旧配置：尚未设置 fallback_model_id）。
	rt := seedFallbackFixture(t, st, up.URL, true)
	fbID := rt.FallbackModelID
	rt.FallbackModelID = 0
	if err := st.DB.Model(&store.Route{}).Where("id = ?", rt.ID).
		Update("fallback_model_id", 0).Error; err != nil {
		t.Fatalf("clear fallback: %v", err)
	}

	if resp := postWithAuth(t, h, chatBody(false), vkToken); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("before config: expect 503, got %d", resp.StatusCode)
	}

	if err := st.DB.Model(&store.Route{}).Where("id = ?", rt.ID).
		Update("fallback_model_id", fbID).Error; err != nil {
		t.Fatalf("set fallback: %v", err)
	}

	resp := postWithAuth(t, h, chatBody(false), vkToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after config: expect 200 from fallback, got %d", resp.StatusCode)
	}
	ls := logs(t, st)
	if ls[1].Model != "m1" || !ls[1].IsFallback {
		t.Fatalf("second log wrong: %+v", ls[1])
	}
}

// TestRouteFallbackAfterHops 主后端有候选但全部失败后，仍应走兜底模型。
func TestRouteFallbackAfterHops(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, "sk-dead") {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m1","choices":[{"message":{"role":"assistant","content":"from fallback"}}]}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "zhipu", BaseURL: up.URL, TimeoutMs: 3000}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m0 := store.Model{ProviderID: p.ID, Name: "m0"}
	if err := st.DB.Create(&m0).Error; err != nil {
		t.Fatal(err)
	}
	k0 := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-dead", Status: "active"}
	if err := st.DB.Create(&k0).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKey{ModelID: m0.ID, KeyID: k0.ID}).Error; err != nil {
		t.Fatal(err)
	}
	m1 := store.Model{ProviderID: p.ID, Name: "m1"}
	if err := st.DB.Create(&m1).Error; err != nil {
		t.Fatal(err)
	}
	k1 := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-alive", Status: "active"}
	if err := st.DB.Create(&k1).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKey{ModelID: m1.ID, KeyID: k1.ID}).Error; err != nil {
		t.Fatal(err)
	}
	rt := store.Route{Name: "glm-pool", FallbackModelID: m1.ID}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m0.ID, Weight: 1}).Error; err != nil {
		t.Fatal(err)
	}

	resp := postWithAuth(t, h, chatBody(false), vkToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expect 200 from fallback after hops, got %d: %s", resp.StatusCode, b)
	}
	ls := logs(t, st)
	if len(ls) != 1 || ls[0].Model != "m1" || !ls[0].IsFallback {
		t.Fatalf("fallback after hops log wrong: %+v", ls)
	}
	if hits < 2 {
		t.Fatalf("expect primary then fallback hits, got %d", hits)
	}
}
