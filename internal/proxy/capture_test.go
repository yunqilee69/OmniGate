package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestContentCaptureOffByDefault：开关关闭时不写 content_log（隐私默认）。
func TestContentCaptureOffByDefault(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer up.Close()

	p := store.Provider{Name: "cap-off-prov", BaseURL: up.URL}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "m"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-1", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := postWithAuth(t, h, chatBody(false), vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("upstream call should succeed, got %d", resp.StatusCode)
	}

	var n int64
	st.DB.Model(&store.ContentLog{}).Count(&n)
	if n != 0 {
		t.Fatalf("capture disabled → content_log must stay empty, got %d rows", n)
	}
}

// TestContentCaptureOnRecordsRequestAndResponse：开启全局开关后 content_log 必须记录
// 请求体（原始 JSON）和响应体（上游回复）。
func TestContentCaptureOnRecordsRequestAndResponse(t *testing.T) {
	st, rtm := newStackWithRTM(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"captured-reply"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	defer up.Close()

	p := store.Provider{Name: "cap-on-prov", BaseURL: up.URL}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "m"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-1", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	if err := rtm.Update(map[string]json.RawMessage{
		"capture.enabled": json.RawMessage(`true`),
	}); err != nil {
		t.Fatalf("enable capture: %v", err)
	}

	resp := post(t, hWithRTM(st, rtm), chatBody(false))
	if resp.StatusCode != 200 {
		t.Fatalf("call should succeed, got %d", resp.StatusCode)
	}

	var cl store.ContentLog
	st.DB.Order("created_at DESC").First(&cl)
	if cl.RequestID == "" {
		t.Fatalf("content_log row missing")
	}
	if cl.Route != "glm-pool" {
		t.Fatalf("route: got %q", cl.Route)
	}
	if !strings.Contains(cl.RequestBody, `"model":"glm-pool"`) || !strings.Contains(cl.RequestBody, `"content":"hello"`) {
		t.Fatalf("request_body missing fields: %s", cl.RequestBody)
	}
	if !strings.Contains(cl.ResponseBody, `"captured-reply"`) {
		t.Fatalf("response_body missing upstream reply: %s", cl.ResponseBody)
	}
}

// TestContentCaptureRouteWhitelist：白名单生效（不在列表的路由不被捕获）。
func TestContentCaptureRouteWhitelist(t *testing.T) {
	st, rtm := newStackWithRTM(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()

	p := store.Provider{Name: "wl-prov", BaseURL: up.URL}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "m"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-1", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	if err := rtm.Update(map[string]json.RawMessage{
		"capture.enabled": json.RawMessage(`true`),
		"capture.routes":  json.RawMessage(`["some-other-route"]`),
	}); err != nil {
		t.Fatalf("enable capture with whitelist: %v", err)
	}

	resp := post(t, hWithRTM(st, rtm), chatBody(false))
	if resp.StatusCode != 200 {
		t.Fatalf("call should succeed, got %d", resp.StatusCode)
	}

	var n int64
	st.DB.Model(&store.ContentLog{}).Count(&n)
	if n != 0 {
		t.Fatalf("whitelist exclude: content_log must be empty, got %d", n)
	}
}
