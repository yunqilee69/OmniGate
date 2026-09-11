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
// 入站请求体（客户端原始提交，model 仍为逻辑路由名）与出站请求体（协议转换/model 替换后实际发送的 JSON）
// 以及上游响应体。
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
	// 入站请求体：客户端提交的原始 JSON，model 仍为逻辑路由名
	if !strings.Contains(cl.ClientRequestBody, `"model":"glm-pool"`) || !strings.Contains(cl.ClientRequestBody, `"content":"hello"`) {
		t.Fatalf("client_request_body missing inbound fields: %s", cl.ClientRequestBody)
	}
	if strings.Contains(cl.ClientRequestBody, `"model":"m"`) {
		t.Fatalf("client_request_body must not contain rewritten physical model: %s", cl.ClientRequestBody)
	}
	// 出站请求体：逻辑路由名 glm-pool 已替换为物理模型名 m
	if !strings.Contains(cl.RequestBody, `"model":"m"`) || !strings.Contains(cl.RequestBody, `"content":"hello"`) {
		t.Fatalf("outbound request_body missing fields: %s", cl.RequestBody)
	}
	if !strings.Contains(cl.ResponseBody, `"captured-reply"`) {
		t.Fatalf("response_body missing upstream reply: %s", cl.ResponseBody)
	}
}

// TestContentCaptureRecordsHeaders：内容捕获需同时记录
// 入站请求头（客户端 → OmniGate，未修改）、出站请求头（OmniGate → 上游，含认证头但脱敏）
// 与上游响应头。
func TestContentCaptureRecordsHeaders(t *testing.T) {
	st, rtm := newStackWithRTM(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Trace", "trace-abc-123")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	defer up.Close()

	p := store.Provider{Name: "hdr-prov", BaseURL: up.URL, HeaderProfile: `{"User-Agent":"hdr-cli/1"}`}
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

	// 客户端带自定义身份头调用；出站侧应替换为提供商请求头设置里的值
	resp := postWithAuth(t, hWithRTM(st, rtm), chatBody(false), "vk-secret-token-abcdefgh")
	if resp.StatusCode != 200 {
		t.Fatalf("call should succeed, got %d", resp.StatusCode)
	}

	var cl store.ContentLog
	st.DB.Order("created_at DESC").First(&cl)
	if cl.RequestID == "" {
		t.Fatalf("content_log row missing")
	}
	// 入站请求头：客户端实际提交（Content-Type + Authorization），未经网关改写
	if !strings.Contains(cl.ClientRequestHeaders, "Content-Type: application/json") {
		t.Fatalf("client_request_headers missing Content-Type: %q", cl.ClientRequestHeaders)
	}
	if !strings.Contains(cl.ClientRequestHeaders, "Authorization: Bearer v****") {
		t.Fatalf("client_request_headers Authorization should be masked: %q", cl.ClientRequestHeaders)
	}
	if strings.Contains(cl.ClientRequestHeaders, "vk-secret-token-abcdefgh") {
		t.Fatalf("client_request_headers must not contain raw credentials: %q", cl.ClientRequestHeaders)
	}
	if strings.Contains(cl.ClientRequestHeaders, "User-Agent: hdr-cli/1") {
		t.Fatalf("client_request_headers must not contain outbound header profile: %q", cl.ClientRequestHeaders)
	}
	// 出站请求头：网关自构（Content-Type + 上游认证头），而非客户端入站头
	if !strings.Contains(cl.RequestHeaders, "Content-Type: application/json") {
		t.Fatalf("outbound request_headers missing Content-Type: %q", cl.RequestHeaders)
	}
	// 出站请求头：上游密钥脱敏为前 8 字符 + ****
	if !strings.Contains(cl.RequestHeaders, "Authorization: Bearer s****") {
		t.Fatalf("outbound request_headers Authorization should be masked: %q", cl.RequestHeaders)
	}
	if strings.Contains(cl.RequestHeaders, "sk-1") || strings.Contains(cl.RequestHeaders, "vk-secret-token-abcdefgh") {
		t.Fatalf("outbound request_headers must not contain raw credentials: %q", cl.RequestHeaders)
	}
	// 出站请求头：提供商请求头设置生效（客户端入站身份头被覆盖）
	if !strings.Contains(cl.RequestHeaders, "User-Agent: hdr-cli/1") {
		t.Fatalf("outbound request_headers missing provider header profile: %q", cl.RequestHeaders)
	}
	// 响应头：来自上游（而非网关自构头）
	if !strings.Contains(cl.ResponseHeaders, "X-Upstream-Trace: trace-abc-123") {
		t.Fatalf("response_headers missing upstream header: %q", cl.ResponseHeaders)
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
