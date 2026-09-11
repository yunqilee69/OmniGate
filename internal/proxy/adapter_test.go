package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

func seedProtocolModel(t *testing.T, st *store.Store, url, name, protocol string) {
	t.Helper()
	p := store.Provider{Name: name + "-prov", BaseURL: url}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: name, Protocol: protocol}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-p", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: name + "-route"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})
}

func anthropicUpstream(t *testing.T, gotReq *map[string]any, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSONBody(r, &body)
		*gotReq = body
		*gotAuth = r.Header.Get("x-api-key")
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"wrong path"}`)
			return
		}
		if _, stream := body["stream"]; stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, s := range []string{
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4","usage":{"input_tokens":25,"output_tokens":1}}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"世界"}}`,
				`data: {"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
				`data: {"type":"message_stop"}`,
			} {
				fmt.Fprintln(w, s)
				fmt.Fprintln(w)
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"anthropic 回答"}],"stop_reason":"end_turn","usage":{"input_tokens":25,"output_tokens":7}}`)
	}))
}

func TestAnthropicBufferedConversion(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var gotReq map[string]any
	var gotAuth string
	up := anthropicUpstream(t, &gotReq, &gotAuth)
	defer up.Close()
	seedProtocolModel(t, st, up.URL+"/v1", "claude-sonnet-4", "messages")

	resp := postWithAuth(t, h, map[string]any{
		"model": "claude-sonnet-4-route",
		"messages": []map[string]any{
			{"role": "system", "content": "你是助手"},
			{"role": "user", "content": "hi"},
		},
	}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotAuth != "sk-p" {
		t.Fatalf("x-api-key auth wrong: %q", gotAuth)
	}
	if gotReq["system"] != "你是助手" {
		t.Fatalf("system not converted: %v", gotReq["system"])
	}
	if gotReq["max_tokens"] == nil {
		t.Fatal("max_tokens must be injected for anthropic")
	}
	msgs := gotReq["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages not converted: %v", msgs)
	}

	body := readAll(t, resp)
	if !strings.Contains(body, `"content":"anthropic 回答"`) {
		t.Fatalf("buffered conversion wrong: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":25`) || !strings.Contains(body, `"completion_tokens":7`) {
		t.Fatalf("usage not converted: %s", body)
	}
	ls := logs(t, st)
	if ls[0].PromptTokens != 25 || ls[0].CompletionTokens != 7 || ls[0].TokensEstimated {
		t.Fatalf("log usage wrong: %+v", ls[0])
	}
}

func TestAnthropicStreamConversion(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var gotReq map[string]any
	var gotAuth string
	up := anthropicUpstream(t, &gotReq, &gotAuth)
	defer up.Close()
	seedProtocolModel(t, st, up.URL+"/v1", "claude-sonnet-4", "messages")

	resp := postWithAuth(t, h, map[string]any{
		"model": "claude-sonnet-4-route", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body := readAll(t, resp)
	for _, want := range []string{
		`"content":"你好"`, `"content":"世界"`, `"finish_reason":"stop"`,
		`"prompt_tokens":25`, `"completion_tokens":7`, `data: [DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream conversion missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "message_start") || strings.Contains(body, "content_block_delta") {
		t.Fatalf("raw anthropic events leaked to client:\n%s", body)
	}
	ls := logs(t, st)
	if ls[0].PromptTokens != 25 || ls[0].CompletionTokens != 7 || !ls[0].IsStream {
		t.Fatalf("stream log usage wrong: %+v", ls[0])
	}
}

// messagesUpstreamPath 仅记录出站路径并返回最小 Anthropic 响应；路径不符时返回 404 供断言失败暴露。
func messagesUpstreamPath(t *testing.T, gotPath *string, wantPath string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		if r.URL.Path != wantPath {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":"wrong path %s"}`, r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
}

// messages 出站与 responses 同规则：直接拼 /messages，版本前缀由 base 自带。
// 回归：base 已含 /v1（用户在提供商里填 https://.../v1）时不得拼成 /v1/v1/messages。
func TestAnthropicMessagesEndpointAppendsSuffix(t *testing.T) {
	tests := []struct {
		name     string
		basePath string
		wantPath string
	}{
		{"bare base", "", "/messages"},
		{"versioned base", "/v1", "/v1/messages"},
		{"versioned base with trailing slash", "/v1/", "/v1/messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, h, vkToken := newTestStackWithVK(t)
			var gotPath string
			up := messagesUpstreamPath(t, &gotPath, tt.wantPath)
			defer up.Close()
			seedProtocolModel(t, st, up.URL+tt.basePath, "claude-sonnet-4", "messages")

			resp := postWithAuth(t, h, map[string]any{
				"model":    "claude-sonnet-4-route",
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			}, vkToken)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d — %s (upstream path %s)", resp.StatusCode, readAll(t, resp), gotPath)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("upstream path = %s, want %s", gotPath, tt.wantPath)
			}
		})
	}
}

// 原生 /v1/messages 直通路径（路由 endpoint=messages）同样不得重复 /v1。
func TestAnthropicNativeMessagesEndpointNoVersionDuplication(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var gotPath string
	up := messagesUpstreamPath(t, &gotPath, "/v1/messages")
	defer up.Close()

	p := store.Provider{Name: "claude-prov", BaseURL: up.URL + "/v1", TimeoutMs: 3000}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "claude", Protocol: "messages", Type: "chat"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-m", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "claude-route", Endpoint: "messages"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	body, _ := json.Marshal(map[string]any{
		"model":      "claude-route",
		"max_tokens": 16,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d — %s (upstream path %s)", resp.StatusCode, readAll(t, resp), gotPath)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %s, want /v1/messages", gotPath)
	}
}

func responsesUpstream(t *testing.T, gotReq *map[string]any, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSONBody(r, &body)
		*gotReq = body
		*gotPath = r.URL.Path
		if r.URL.Path != "/responses" {
			w.WriteHeader(404)
			return
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, s := range []string{
				`data: {"type":"response.created","response":{"id":"resp_01"}}`,
				`data: {"type":"response.output_text.delta","delta":"Resp你好"}`,
				`data: {"type":"response.output_text.delta","delta":"Resp世界"}`,
				`data: {"type":"response.completed","response":{"id":"resp_01","usage":{"input_tokens":31,"output_tokens":9,"total_tokens":40}}}`,
			} {
				fmt.Fprintln(w, s)
				fmt.Fprintln(w)
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_01","object":"response","status":"completed","model":"gpt-5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses 回答"}]}],"usage":{"input_tokens":31,"output_tokens":9,"total_tokens":40}}`)
	}))
}

func TestResponsesBufferedConversion(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var gotReq map[string]any
	var gotPath string
	up := responsesUpstream(t, &gotReq, &gotPath)
	defer up.Close()
	seedProtocolModel(t, st, up.URL, "gpt-5", "responses")

	resp := postWithAuth(t, h, map[string]any{
		"model":      "gpt-5-route",
		"max_tokens": 777,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotPath != "/responses" {
		t.Fatalf("wrong endpoint: %s", gotPath)
	}
	if gotReq["input"] == nil || gotReq["max_output_tokens"] != float64(777) {
		t.Fatalf("request not converted: %v", gotReq)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, `"content":"responses 回答"`) {
		t.Fatalf("conversion wrong: %s", body)
	}
	ls := logs(t, st)
	if ls[0].PromptTokens != 31 || ls[0].CompletionTokens != 9 {
		t.Fatalf("log usage wrong: %+v", ls[0])
	}
}

func TestResponsesStreamConversion(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var gotReq map[string]any
	var gotPath string
	up := responsesUpstream(t, &gotReq, &gotPath)
	defer up.Close()
	seedProtocolModel(t, st, up.URL, "gpt-5", "responses")

	resp := postWithAuth(t, h, map[string]any{
		"model": "gpt-5-route", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body := readAll(t, resp)
	for _, want := range []string{
		`"content":"Resp你好"`, `"content":"Resp世界"`, `"finish_reason":"stop"`,
		`"prompt_tokens":31`, `"completion_tokens":9`, `data: [DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream conversion missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "response.output_text.delta") {
		t.Fatalf("raw responses events leaked to client:\n%s", body)
	}
	ls := logs(t, st)
	if ls[0].PromptTokens != 31 || ls[0].CompletionTokens != 9 || !ls[0].IsStream {
		t.Fatalf("stream log usage wrong: %+v", ls[0])
	}
}

// anthropicThinkingUpstream 回带 thinking 块的 Anthropic mock：流式（thinking_delta）与缓冲（thinking 块）两条路径。
func anthropicThinkingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSONBody(r, &body)
		if _, stream := body["stream"]; stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, s := range []string{
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_t","model":"claude-sonnet-4","usage":{"input_tokens":25,"output_tokens":1}}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"后想"}}`,
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"正答"}}`,
				`data: {"type":"message_delta","delta":{"type":"message_delta","stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
				`data: {"type":"message_stop"}`,
			} {
				fmt.Fprintln(w, s)
				fmt.Fprintln(w)
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_t","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"thinking","thinking":"思考内容","signature":"sig"},{"type":"text","text":"anthropic 回答"}],"stop_reason":"end_turn","usage":{"input_tokens":25,"output_tokens":9}}`)
	}))
}

func TestAnthropicThinkingConversion(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := anthropicThinkingUpstream(t)
	defer up.Close()
	seedProtocolModel(t, st, up.URL+"/v1", "claude-think", "messages")

	// 流式：thinking_delta → delta.reasoning_content，thinking 原始事件不得泄漏
	resp := postWithAuth(t, h, map[string]any{
		"model": "claude-think-route", "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	body := readAll(t, resp)
	for _, want := range []string{
		`"reasoning_content":"先想"`, `"reasoning_content":"后想"`, `"content":"正答"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream thinking conversion missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "thinking_delta") || strings.Contains(body, `"thinking"`) {
		t.Fatalf("raw anthropic thinking events leaked to client:\n%s", body)
	}

	// 缓冲：thinking 块 → message.reasoning_content
	resp2 := postWithAuth(t, h, map[string]any{
		"model":    "claude-think-route",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, vkToken)
	if resp2.StatusCode != 200 {
		t.Fatalf("buffered status %d", resp2.StatusCode)
	}
	body2 := readAll(t, resp2)
	for _, want := range []string{`"reasoning_content":"思考内容"`, `"content":"anthropic 回答"`} {
		if !strings.Contains(body2, want) {
			t.Fatalf("buffered thinking conversion missing %q:\n%s", want, body2)
		}
	}
	if strings.Contains(body2, "signature") {
		t.Fatalf("thinking signature leaked to client:\n%s", body2)
	}
	_ = st
}
