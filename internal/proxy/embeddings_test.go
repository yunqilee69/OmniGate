package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudomni/omnigate/internal/proxy"
	"github.com/cloudomni/omnigate/internal/store"
)

// seedTypedRoute 建一个含 chat + embedding + rerank 三类后端的路由 "mixed"。
// chat 后端一旦被非 chat 端点选中会直接让测试失败（返回 500），用于验证类型过滤。
func seedTypedRoute(t *testing.T, st *store.Store, baseURL string) {
	t.Helper()
	p := store.Provider{Name: "prov", BaseURL: baseURL}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	mk := func(name, mtype string) store.Model {
		m := store.Model{ProviderID: p.ID, Name: name, Type: mtype, InputPrice: 10, OutputPrice: 20}
		if err := st.DB.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
		k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-e-" + name, Status: "active"}
		if err := st.DB.Create(&k).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID}).Error; err != nil {
			t.Fatal(err)
		}
		return m
	}
	mChat := mk("gpt-chat", "chat")
	mEmb := mk("text-embedding-3-small", "embedding")
	mRrk := mk("bge-reranker-v2-m3", "rerank")
	rt := store.Route{Name: "mixed"}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.Model{mChat, mEmb, mRrk} {
		if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 100}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func typedPost(t *testing.T, h http.Handler, path string, body any) *http.Response {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestEmbeddingsProxy(t *testing.T) {
	st, h := newTestStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("upstream path = %s, want /v1/embeddings", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-e-text-embedding-3-small" {
			t.Errorf("auth header = %q", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "text-embedding-3-small" {
			t.Errorf("upstream model = %v, want physical name", body["model"])
		}
		if body["input"] != "hello" {
			t.Errorf("input not passed through: %v", body["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":5,"total_tokens":5}}`)
	}))
	defer up.Close()
	seedTypedRoute(t, st, up.URL+"/v1")

	resp := typedPost(t, h, "/v1/embeddings", map[string]any{"model": "mixed", "input": "hello"})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "list" {
		t.Errorf("response not passed through: %v", out)
	}
	rows := logs(t, st)
	if len(rows) != 1 {
		t.Fatalf("request_log rows = %d, want 1", len(rows))
	}
	lg := rows[0]
	if lg.Route != "mixed" || lg.Model != "text-embedding-3-small" || lg.PromptTokens != 5 || lg.CompletionTokens != 0 || lg.IsStream {
		t.Errorf("log mismatch: %+v", lg)
	}
	if lg.Cost <= 0 {
		t.Errorf("cost not computed: %v", lg.Cost)
	}
}

func TestRerankProxy(t *testing.T) {
	st, h := newTestStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("upstream path = %s, want /v1/rerank", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "bge-reranker-v2-m3" {
			t.Errorf("upstream model = %v", body["model"])
		}
		if _, ok := body["documents"].([]any); !ok {
			t.Errorf("documents not passed through: %v", body["documents"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"rr-1","results":[{"index":0,"relevance_score":0.98},{"index":1,"relevance_score":0.11}],"meta":{"tokens":{"input_tokens":42,"output_tokens":0}}}`)
	}))
	defer up.Close()
	seedTypedRoute(t, st, up.URL+"/v1")

	resp := typedPost(t, h, "/v1/rerank", map[string]any{
		"model": "mixed", "query": "什么是网关", "documents": []string{"OmniGate 是网关", "无关文本"}, "top_n": 2,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["results"].([]any); !ok {
		t.Errorf("cohere response not passed through: %v", out)
	}
	rows := logs(t, st)
	if len(rows) != 1 || rows[0].PromptTokens != 42 || rows[0].Model != "bge-reranker-v2-m3" {
		t.Fatalf("rerank usage not recorded: %+v", rows)
	}
}

func TestTypedEndpointsFilterModelType(t *testing.T) {
	st, h := newTestStack(t)
	var embModels []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			embModels = append(embModels, body["model"].(string))
			fmt.Fprint(w, `{"object":"list","data":[],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
		default:
			t.Errorf("chat/rerank model leaked into embeddings call: path=%s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer up.Close()
	seedTypedRoute(t, st, up.URL+"/v1")

	// chat 后端权重与 embedding 相同(100)，若类型过滤失效大概率命中 chat 模型；
	// 多次调用保证确定性排除偶然
	for i := 0; i < 20; i++ {
		resp := typedPost(t, h, "/v1/embeddings", map[string]any{"model": "mixed", "input": "x"})
		if resp.StatusCode != 200 {
			t.Fatalf("iter %d: status = %d", i, resp.StatusCode)
		}
	}
	for _, m := range embModels {
		if m != "text-embedding-3-small" {
			t.Fatalf("non-embedding model picked: %s", m)
		}
	}
	if len(embModels) != 20 {
		t.Fatalf("calls reached upstream %d times, want 20", len(embModels))
	}

	// 反向：仅含 chat 模型的路由打 embeddings 端点 → all_backends
	p := store.Provider{Name: "chatonly-prov", BaseURL: up.URL + "/v1"}
	st.DB.Create(&p)
	mc := store.Model{ProviderID: p.ID, Name: "chat-only", Type: "chat"}
	st.DB.Create(&mc)
	kc := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-c", Status: "active"}
	st.DB.Create(&kc)
	st.DB.Create(&store.ModelKey{ModelID: mc.ID, KeyID: kc.ID})
	r2 := store.Route{Name: "chatonly"}
	st.DB.Create(&r2)
	st.DB.Create(&store.RouteTarget{RouteID: r2.ID, ModelID: mc.ID, Weight: 1})

	resp := typedPost(t, h, "/v1/embeddings", map[string]any{"model": "chatonly", "input": "x"})
	if resp.StatusCode != 503 {
		t.Fatalf("chat-only route on embeddings: status = %d, want 503", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "all_backends") {
		t.Errorf("want all_backends error, got %s", b)
	}
}

func TestTypedEndpointValidation(t *testing.T) {
	_, h := newTestStack(t)
	if resp := typedPost(t, h, "/v1/embeddings", map[string]any{"input": "x"}); resp.StatusCode != 400 {
		t.Errorf("missing model: status = %d", resp.StatusCode)
	}
	if resp := typedPost(t, h, "/v1/rerank", map[string]any{"model": "nope", "query": "q"}); resp.StatusCode != 404 {
		t.Errorf("unknown route: status = %d", resp.StatusCode)
	}
	if resp := typedPost(t, h, "/v1/embeddings", "not-json"); resp.StatusCode != 400 {
		t.Errorf("bad json: status = %d", resp.StatusCode)
	}
}

// 编译期保证 Handler 实现 api.TypedPlane。
var _ interface {
	Embeddings(w http.ResponseWriter, r *http.Request)
	Rerank(w http.ResponseWriter, r *http.Request)
} = (*proxy.Handler)(nil)
