package proxy_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestApiPathOverride 验证 Model.ApiPath 可以覆盖默认端点 URL
func TestApiPathOverride(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	var gotPath string
	var gotReq map[string]any
	
	// 创建一个自定义端点的 upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = decodeJSONBody(r, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-123","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"custom path response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer upstream.Close()

	// 创建带自定义 ApiPath 的模型
	p := store.Provider{Name: "custom-provider", BaseURL: upstream.URL}
	st.DB.Create(&p)
	m := store.Model{
		ProviderID: p.ID,
		Name:       "custom-model",
		Protocol:   "completions",
		ApiPath:    upstream.URL + "/my/custom/endpoint", // 完整的自定义路径
	}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-custom", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "custom-route", Endpoint: "completions"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	// 发送请求
	body := `{"model":"custom-route","messages":[{"role":"user","content":"test"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+vkToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 验证请求到了自定义路径
	if gotPath != "/my/custom/endpoint" {
		t.Errorf("expected path /my/custom/endpoint, got %s", gotPath)
	}

	// 验证响应包含自定义端点的回复
	if !strings.Contains(rec.Body.String(), "custom path response") {
		t.Errorf("expected custom path response, got: %s", rec.Body.String())
	}
}

// TestBodyOverrideModel 验证 Model.BodyOverride 可以覆盖请求体字段
func TestBodyOverrideModel(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	var gotReq map[string]any
	
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = decodeJSONBody(r, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-123","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"overridden"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer upstream.Close()

	// 创建带 BodyOverride 的模型
	p := store.Provider{Name: "override-provider", BaseURL: upstream.URL}
	st.DB.Create(&p)
	m := store.Model{
		ProviderID:   p.ID,
		Name:         "override-model",
		Protocol:     "completions",
		BodyOverride: `{"temperature": 0.123, "max_tokens": 999, "custom_field": "custom_value"}`,
	}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-override", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "override-route", Endpoint: "completions"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	// 发送请求（带原始 temperature）
	body := `{"model":"override-route","messages":[{"role":"user","content":"test"}],"temperature":0.7}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+vkToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 验证 BodyOverride 覆盖了字段
	if temp, ok := gotReq["temperature"].(float64); !ok || temp != 0.123 {
		t.Errorf("expected temperature 0.123, got %v", gotReq["temperature"])
	}
	if maxTokens, ok := gotReq["max_tokens"].(float64); !ok || maxTokens != 999 {
		t.Errorf("expected max_tokens 999, got %v", gotReq["max_tokens"])
	}
	if custom, ok := gotReq["custom_field"].(string); !ok || custom != "custom_value" {
		t.Errorf("expected custom_field 'custom_value', got %v", gotReq["custom_field"])
	}
}

// TestProtocolRename 验证协议值已重命名并正常工作
func TestProtocolRename(t *testing.T) {
	st, _, _ := newTestStackWithVK(t)

	// 测试新协议名称
	protocols := []string{"completions", "messages", "responses"}
	
	for _, proto := range protocols {
		t.Run(proto, func(t *testing.T) {
			p := store.Provider{Name: proto + "-provider", BaseURL: "https://example.com"}
			st.DB.Create(&p)
			m := store.Model{
				ProviderID: p.ID,
				Name:       proto + "-model",
				Protocol:   proto,
			}
			if err := st.DB.Create(&m).Error; err != nil {
				t.Fatalf("failed to create model with protocol %s: %v", proto, err)
			}
			
			// 验证模型已创建且协议值正确
			var loaded store.Model
			st.DB.First(&loaded, m.ID)
			if loaded.Protocol != proto {
				t.Errorf("expected protocol %s, got %s", proto, loaded.Protocol)
			}
		})
	}
}
