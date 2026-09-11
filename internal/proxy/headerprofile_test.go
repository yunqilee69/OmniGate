package proxy_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// newEchoUpstream 记录收到的请求头并返回合法 chat 响应，供断言上游实际收到的头。
func newEchoUpstream(t *testing.T, got *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
}

// seedProfileStack 与 setupSingleUpstream 同构，提供商额外带客户端模拟请求头字段。
func seedProfileStack(t *testing.T, st *store.Store, url, headerProfile string) {
	t.Helper()
	p := store.Provider{Name: "zhipu", BaseURL: url, TimeoutMs: 5000, HeaderProfile: headerProfile}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-to", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})
}

// postCustomHeaders 发送非流式 chat 请求，extra 允许设置额外入站头（在 VK 鉴权头之后）。
func postCustomHeaders(t *testing.T, h http.Handler, vkToken string, extra func(*http.Request)) *http.Response {
	t.Helper()
	buf, _ := json.Marshal(chatBody(false))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if vkToken != "" {
		req.Header.Set("Authorization", "Bearer "+vkToken)
	}
	if extra != nil {
		extra(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// 已配置模拟头：上游收到模拟头；保留头 Authorization 被跳过（保留适配器认证值）。
func TestHeaderProfileOverridesUpstreamHeaders(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var got http.Header
	up := newEchoUpstream(t, &got)
	defer up.Close()
	seedProfileStack(t, st, up.URL,
		`{"User-Agent":"fake-cli/1.0","X-App":"cli","Authorization":"Bearer spoof"}`)

	resp := postCustomHeaders(t, h, vkToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d", resp.StatusCode)
	}
	if got.Get("User-Agent") != "fake-cli/1.0" {
		t.Errorf("User-Agent = %q, want fake-cli/1.0", got.Get("User-Agent"))
	}
	if got.Get("X-App") != "cli" {
		t.Errorf("X-App = %q, want cli", got.Get("X-App"))
	}
	if got.Get("Authorization") != "Bearer sk-to" {
		t.Errorf("reserved Authorization must keep adapter value, got %q", got.Get("Authorization"))
	}
}

// 未选组：入站白名单身份头透传；Cookie 与虚拟密钥 Authorization 不透传。
func TestInboundIdentityAllowlistPassthrough(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var got http.Header
	up := newEchoUpstream(t, &got)
	defer up.Close()
	seedProfileStack(t, st, up.URL, "")

	resp := postCustomHeaders(t, h, vkToken, func(req *http.Request) {
		req.Header.Set("User-Agent", "test-cli/1")
		req.Header.Set("X-Stainless-OS", "macOS")
		req.Header.Set("Cookie", "session=xyz")
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d", resp.StatusCode)
	}
	if got.Get("User-Agent") != "test-cli/1" {
		t.Errorf("User-Agent = %q, want passthrough test-cli/1", got.Get("User-Agent"))
	}
	if got.Get("X-Stainless-OS") != "macOS" {
		t.Errorf("X-Stainless-OS = %q, want passthrough macOS", got.Get("X-Stainless-OS"))
	}
	if got.Get("Cookie") != "" {
		t.Errorf("Cookie must not be forwarded, got %q", got.Get("Cookie"))
	}
	if got.Get("Authorization") != "Bearer sk-to" {
		t.Errorf("inbound Authorization (VK token) must not be forwarded, got %q", got.Get("Authorization"))
	}
}

// 未选组且入站无白名单头：上游 UA 为 Go 默认。
func TestNoProfileNoInboundIdentityKeepsDefaultUA(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var got http.Header
	up := newEchoUpstream(t, &got)
	defer up.Close()
	seedProfileStack(t, st, up.URL, "")

	resp := postCustomHeaders(t, h, vkToken, func(req *http.Request) {
		req.Header.Del("User-Agent")
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d", resp.StatusCode)
	}
	if got.Get("User-Agent") != "Go-http-client/1.1" {
		t.Errorf("User-Agent = %q, want Go default", got.Get("User-Agent"))
	}
}

// header_profile JSON 非法：告警后静默回退透传，不报错。
func TestMalformedProfileFallsBackToPassthrough(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var got http.Header
	up := newEchoUpstream(t, &got)
	defer up.Close()
	seedProfileStack(t, st, up.URL, `{"User-Agent": oops`)

	resp := postCustomHeaders(t, h, vkToken, func(req *http.Request) {
		req.Header.Set("User-Agent", "test-cli/1")
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d", resp.StatusCode)
	}
	if got.Get("User-Agent") != "test-cli/1" {
		t.Errorf("User-Agent = %q, want passthrough fallback test-cli/1", got.Get("User-Agent"))
	}
}
