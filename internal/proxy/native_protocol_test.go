package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

func TestNativeMessagesFiltersProtocol(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	var completionsHits, messagesHits int32
	completionsUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&completionsHits, 1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"openai"}}]}`)
	}))
	defer completionsUp.Close()
	messagesUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&messagesHits, 1)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("messages upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"claude"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer messagesUp.Close()

	pC := store.Provider{Name: "openai-prov", BaseURL: completionsUp.URL, TimeoutMs: 3000}
	st.DB.Create(&pC)
	mC := store.Model{ProviderID: pC.ID, Name: "gpt", Protocol: "completions", Type: "chat"}
	st.DB.Create(&mC)
	kC := store.ApiKey{ProviderID: pC.ID, KeyValue: "sk-c", Status: "active"}
	st.DB.Create(&kC)
	st.DB.Create(&store.ModelKey{ModelID: mC.ID, KeyID: kC.ID})

	pM := store.Provider{Name: "claude-prov", BaseURL: messagesUp.URL + "/v1", TimeoutMs: 3000}
	st.DB.Create(&pM)
	mM := store.Model{ProviderID: pM.ID, Name: "claude", Protocol: "messages", Type: "chat"}
	st.DB.Create(&mM)
	kM := store.ApiKey{ProviderID: pM.ID, KeyValue: "sk-m", Status: "active"}
	st.DB.Create(&kM)
	st.DB.Create(&store.ModelKey{ModelID: mM.ID, KeyID: kM.ID})

	rt := store.Route{Name: "mixed", Endpoint: "messages"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mC.ID, Weight: 999})
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mM.ID, Weight: 1})

	body, _ := json.Marshal(map[string]any{
		"model":      "mixed",
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
		t.Fatalf("status %d — %s", resp.StatusCode, readAll(t, resp))
	}
	if atomic.LoadInt32(&completionsHits) != 0 {
		t.Fatalf("/v1/messages hit completions upstream %d times", completionsHits)
	}
	if atomic.LoadInt32(&messagesHits) != 1 {
		t.Fatalf("messages upstream hits=%d, want 1", messagesHits)
	}
}

func TestNativeResponsesFiltersProtocol(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)

	var completionsHits, responsesHits int32
	completionsUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&completionsHits, 1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"openai"}}]}`)
	}))
	defer completionsUp.Close()
	responsesUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&responsesHits, 1)
		if r.URL.Path != "/v1/responses" && r.URL.Path != "/responses" {
			// AdapterFor("responses") uses /responses or /v1/responses depending on base
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
	}))
	defer responsesUp.Close()

	pC := store.Provider{Name: "openai-prov", BaseURL: completionsUp.URL, TimeoutMs: 3000}
	st.DB.Create(&pC)
	mC := store.Model{ProviderID: pC.ID, Name: "gpt", Protocol: "completions", Type: "chat"}
	st.DB.Create(&mC)
	kC := store.ApiKey{ProviderID: pC.ID, KeyValue: "sk-c", Status: "active"}
	st.DB.Create(&kC)
	st.DB.Create(&store.ModelKey{ModelID: mC.ID, KeyID: kC.ID})

	pR := store.Provider{Name: "o-prov", BaseURL: responsesUp.URL, TimeoutMs: 3000}
	st.DB.Create(&pR)
	mR := store.Model{ProviderID: pR.ID, Name: "o3", Protocol: "responses", Type: "chat"}
	st.DB.Create(&mR)
	kR := store.ApiKey{ProviderID: pR.ID, KeyValue: "sk-r", Status: "active"}
	st.DB.Create(&kR)
	st.DB.Create(&store.ModelKey{ModelID: mR.ID, KeyID: kR.ID})

	rt := store.Route{Name: "mixed-resp", Endpoint: "responses"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mC.ID, Weight: 999})
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mR.ID, Weight: 1})

	body, _ := json.Marshal(map[string]any{
		"model": "mixed-resp",
		"input": "hi",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d — %s", resp.StatusCode, b)
	}
	if atomic.LoadInt32(&completionsHits) != 0 {
		t.Fatalf("/v1/responses hit completions upstream %d times", completionsHits)
	}
	if atomic.LoadInt32(&responsesHits) != 1 {
		t.Fatalf("responses upstream hits=%d, want 1", responsesHits)
	}
}

func TestNativeMessagesProviderModelRewritesBody(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "anthropic", BaseURL: up.URL + "/v1", TimeoutMs: 3000}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "claude-sonnet-4", Protocol: "messages", Type: "chat"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-ant", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})

	body, _ := json.Marshal(map[string]any{
		"model":      "anthropic/claude-sonnet-4",
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
		t.Fatalf("status %d — %s", resp.StatusCode, readAll(t, resp))
	}
	if gotModel != "claude-sonnet-4" {
		t.Fatalf("upstream model = %q, want physical name", gotModel)
	}
}

func TestNativeMessagesProviderModelEndpointMismatch(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	p := store.Provider{Name: "zhipu", BaseURL: "http://127.0.0.1:1"}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "glm-4.6", Protocol: "completions", Type: "chat"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-z", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})

	body, _ := json.Marshal(map[string]any{
		"model":      "zhipu/glm-4.6",
		"max_tokens": 16,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("completions model on /v1/messages: status=%d, want 400", rec.Code)
	}
}

func TestNativeMessagesStreamCapturesUsage(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, s := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":4,"cache_creation_input_tokens":6}}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			`data: {"type":"message_stop"}`,
		} {
			fmt.Fprintln(w, s)
			fmt.Fprintln(w)
			fl.Flush()
		}
	}))
	defer up.Close()
	p := store.Provider{Name: "claude-stream", BaseURL: up.URL + "/v1", TimeoutMs: 3000}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "claude", Protocol: "messages", Type: "chat", InputPrice: 10, CachedPrice: 2, OutputPrice: 20}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-m", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "claude-stream", Endpoint: "messages"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	body, _ := json.Marshal(map[string]any{
		"model": "claude-stream", "stream": true, "max_tokens": 16,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d — %s", rec.Code, rec.Body.String())
	}
	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	if ls[0].TokensEstimated {
		t.Fatalf("native stream must capture usage, not estimate: %+v", ls[0])
	}
	if ls[0].PromptTokens != 35 || ls[0].CachedTokens != 4 || ls[0].CompletionTokens != 7 {
		t.Fatalf("native stream usage wrong: %+v", ls[0])
	}
}

// TestNativeMessagesStreamHangCloseNotSuccess message_start 已带 input_tokens，
// 上游随即断开：不得把半截流记成 success。
func TestNativeMessagesStreamHangCloseNotSuccess(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintln(w, `event: message_start`)
		fmt.Fprintln(w, `data: {"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":25,"output_tokens":1}}}`)
		fmt.Fprintln(w)
		fl.Flush()
	}))
	defer up.Close()
	p := store.Provider{Name: "claude-hang", BaseURL: up.URL + "/v1", TimeoutMs: 3000}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "claude", Protocol: "messages", Type: "chat", InputPrice: 10, OutputPrice: 20}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-m", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "claude-hang", Endpoint: "messages"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	body, _ := json.Marshal(map[string]any{
		"model": "claude-hang", "stream": true, "max_tokens": 16,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("committed native stream status %d — %s", rec.Code, rec.Body.String())
	}
	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	if ls[0].Status != "error" || ls[0].ErrorCode != "stream_broken" {
		t.Fatalf("truncated anthropic stream must be stream_broken, got %+v", ls[0])
	}
	if ls[0].Cost != 0 {
		t.Fatalf("truncated anthropic stream must not bill, cost=%v", ls[0].Cost)
	}
}
