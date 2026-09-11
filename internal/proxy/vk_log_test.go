package proxy_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

func seedChatRoute(t *testing.T, st *store.Store, name, baseURL string) store.Route {
	t.Helper()
	p := store.Provider{Name: name + "-prov", BaseURL: baseURL, TimeoutMs: 3000}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m := store.Model{ProviderID: p.ID, Name: name + "-model", Protocol: "completions"}
	if err := st.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-" + name, Status: "active"}
	if err := st.DB.Create(&k).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID}).Error; err != nil {
		t.Fatal(err)
	}
	rt := store.Route{Name: name, Endpoint: "completions"}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1}).Error; err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestRequestLogRecordsVKID(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	seedChatRoute(t, st, "glm-pool", up.URL)

	vk, err := st.GetVirtualKeyByValue(vkToken)
	if err != nil {
		t.Fatal(err)
	}

	resp := postWithAuth(t, h, chatBody(false), vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d — %s", resp.StatusCode, readAll(t, resp))
	}

	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log row, got %d", len(ls))
	}
	if ls[0].VKID != vk.ID {
		t.Fatalf("vk_id = %d, want %d", ls[0].VKID, vk.ID)
	}

	updated, err := st.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TotalRequests != 1 {
		t.Fatalf("VK usage settlement did not run: total_requests=%d", updated.TotalRequests)
	}
}

func TestVKRouteAllowlistDenies(t *testing.T) {
	st, h, openToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	rt := seedChatRoute(t, st, "glm-pool", up.URL)

	denied := &store.VirtualKey{
		Name:          "denied",
		Status:        "active",
		AllowedRoutes: `[99999]`,
	}
	if err := st.CreateVirtualKey(denied); err != nil {
		t.Fatal(err)
	}

	resp := postWithAuth(t, h, chatBody(false), denied.KeyValue)
	if resp.StatusCode != 403 {
		t.Fatalf("allowlist miss should 403, got %d — %s", resp.StatusCode, readAll(t, resp))
	}

	allowed := &store.VirtualKey{
		Name:          "allowed",
		Status:        "active",
		AllowedRoutes: fmt.Sprintf("[%d]", rt.ID),
	}
	if err := st.CreateVirtualKey(allowed); err != nil {
		t.Fatal(err)
	}
	resp = postWithAuth(t, h, chatBody(false), allowed.KeyValue)
	if resp.StatusCode != 200 {
		t.Fatalf("allowlist hit should 200, got %d — %s", resp.StatusCode, readAll(t, resp))
	}

	resp = postWithAuth(t, h, chatBody(false), openToken)
	if resp.StatusCode != 200 {
		t.Fatalf("empty allowlist should 200, got %d — %s", resp.StatusCode, readAll(t, resp))
	}
}

func TestRequestLogPreservesCreatedAt(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	seedChatRoute(t, st, "glm-pool", up.URL)

	before := time.Now().Unix()
	resp := postWithAuth(t, h, chatBody(false), vkToken)
	after := time.Now().Unix()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d — %s", resp.StatusCode, readAll(t, resp))
	}

	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log row, got %d", len(ls))
	}
	if ls[0].CreatedAt == 0 {
		t.Fatal("created_at wiped to 0 by Save")
	}
	if ls[0].CreatedAt < before-1 || ls[0].CreatedAt > after+1 {
		t.Fatalf("created_at=%d not in [%d, %d]", ls[0].CreatedAt, before, after)
	}
	if ls[0].Status != "success" {
		t.Fatalf("status=%s, want success", ls[0].Status)
	}
}

func TestFailoverDailyCountsFinalOnly(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"code":"first_fail"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "fa-prov", BaseURL: up.URL, TimeoutMs: 3000}
	st.DB.Create(&p)
	k1 := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-1", Status: "active"}
	k2 := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-2", Status: "active"}
	st.DB.Create(&k1)
	st.DB.Create(&k2)
	m := store.Model{ProviderID: p.ID, Name: "m", Protocol: "completions"}
	st.DB.Create(&m)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k1.ID})
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k2.ID})
	rt := store.Route{Name: "r", Endpoint: "completions"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := postWithAuth(t, h, map[string]any{
		"model":    "r",
		"messages": []map[string]any{{"role": "user", "content": "x"}},
	}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("retry should succeed, got %d — %s", resp.StatusCode, readAll(t, resp))
	}

	var daily []store.RequestLogDaily
	if err := st.DB.Find(&daily).Error; err != nil {
		t.Fatal(err)
	}
	var successTotal, errorTotal int64
	for _, d := range daily {
		switch d.Status {
		case "success":
			successTotal += d.Total
		case "error", "pending":
			errorTotal += d.Total
		}
	}
	if successTotal != 1 {
		t.Fatalf("daily success total=%d, want 1; rows=%+v", successTotal, daily)
	}
	if errorTotal != 0 {
		t.Fatalf("intermediate hops must not roll up as errors, errorTotal=%d rows=%+v", errorTotal, daily)
	}

	ls := logs(t, st)
	if len(ls) != 1 || ls[0].Status != "success" {
		t.Fatalf("request_log final row wrong: %+v", ls)
	}
}

func TestPendingNotRolledIntoDaily(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	seedChatRoute(t, st, "glm-pool", up.URL)

	done := make(chan *http.Response, 1)
	go func() {
		done <- postWithAuth(t, h, chatBody(false), vkToken)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not hit")
	}

	var daily []store.RequestLogDaily
	if err := st.DB.Find(&daily).Error; err != nil {
		t.Fatal(err)
	}
	for _, d := range daily {
		if d.Status == "pending" || d.Errors > 0 {
			t.Fatalf("pending must not enter daily: %+v", d)
		}
	}

	close(release)
	resp := <-done
	if resp.StatusCode != 200 {
		t.Fatalf("status %d — %s", resp.StatusCode, readAll(t, resp))
	}
	_ = json.RawMessage(nil)
}

// TestVKUsageRecordedForTypedEndpoint 回归：typed 端点（embeddings）的成功请求必须结算 VK
// 用量。此前只有 chat 端点扣费，typed/native 端点只查预算不扣费，预算形同虚设。
func TestVKUsageRecordedForTypedEndpoint(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":5,"total_tokens":5}}`)
	}))
	defer up.Close()
	seedTypedRoute(t, st, up.URL)

	resp := typedPost(t, h, "/v1/embeddings", map[string]any{"model": "mixed", "input": "hello"}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d — %s", resp.StatusCode, readAll(t, resp))
	}
	vk, err := st.GetVirtualKeyByValue(vkToken)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TotalRequests != 1 {
		t.Fatalf("typed success must record vk usage: total_requests=%d", updated.TotalRequests)
	}
	wantCost := 5 * 10 / 1e6 // input 5 tokens × $10/1M
	if updated.UsedUSD != wantCost {
		t.Fatalf("used_usd = %v, want %v", updated.UsedUSD, wantCost)
	}
}
