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

// seedTypedRoute 建一个含 chat + embedding + rerank + image 四类后端的路由 "mixed"。
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
	mImg := mk("cogview-4", "image")
	rt := store.Route{Name: "mixed"}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.Model{mChat, mEmb, mRrk, mImg} {
		if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 100}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func typedPost(t *testing.T, h http.Handler, path string, body any, vkToken string) *http.Response {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestEmbeddingsProxy(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
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
	resp := typedPost(t, h, "/v1/embeddings", map[string]any{"model": "mixed", "input": "hello"}, vkToken)
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
	st, h, vkToken := newTestStackWithVK(t)
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
	}, vkToken)
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

func TestImagesProxy(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var gotStream bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("upstream path = %s, want /v1/images/generations", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-e-cogview-4" {
			t.Errorf("auth header = %q", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "cogview-4" {
			t.Errorf("upstream model = %v, want physical name", body["model"])
		}
		if body["prompt"] != "a cat" {
			t.Errorf("upstream prompt = %v", body["prompt"])
		}
		gotStream = body["stream"] == true
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"created":1789000000,"data":[{"url":"https://cdn.example.com/img.png"}],"usage":{"input_tokens":13,"output_tokens":4096,"total_tokens":4109}}`)
	}))
	defer up.Close()
	seedTypedRoute(t, st, up.URL+"/v1")

	// 非流式：响应直通，usage 按 OpenAI Images 格式落库
	resp := typedPost(t, h, "/v1/images/generations", map[string]any{"model": "mixed", "prompt": "a cat"}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil || len(out.Data) != 1 || out.Data[0].URL == "" {
		t.Fatalf("image response not relayed: %s", b)
	}

	// stream 字段原样透传：上游收到 stream:true，客户端拿到上游响应
	resp = typedPost(t, h, "/v1/images/generations", map[string]any{"model": "mixed", "prompt": "a cat", "stream": true}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	if !gotStream {
		t.Fatalf("stream flag not forwarded to upstream")
	}

	rows := logs(t, st)
	if len(rows) != 2 || rows[0].Model != "cogview-4" || rows[0].PromptTokens != 13 || rows[0].CompletionTokens != 4096 || rows[0].IsStream {
		t.Fatalf("image usage not recorded: %+v", rows)
	}
}

// 生图路径同样应用模型级 BodyOverride（与 chat 一致：覆盖优先于客户端同名字段，
// 其余客户端字段如 quality/prompt 原样透传）。高清预设如 {"size":"4K"} 由此下发。
func TestImagesBodyOverride(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	var gotSize, gotQuality, gotPrompt, gotModel any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSize, gotQuality, gotPrompt, gotModel = body["size"], body["quality"], body["prompt"], body["model"]
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"created":1,"data":[{"url":"https://cdn.example.com/i.png"}]}`)
	}))
	defer up.Close()
	p := store.Provider{Name: "ovr-prov", BaseURL: up.URL + "/v1"}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m := store.Model{ProviderID: p.ID, Name: "seedream-4", Type: "image", BodyOverride: `{"size":"4K"}`}
	if err := st.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-ovr", Status: "active"}
	if err := st.DB.Create(&k).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID}).Error; err != nil {
		t.Fatal(err)
	}
	rt := store.Route{Name: "img-ovr"}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1}).Error; err != nil {
		t.Fatal(err)
	}

	resp := typedPost(t, h, "/v1/images/generations", map[string]any{
		"model": "img-ovr", "prompt": "a cat", "size": "1024x1024", "quality": "high",
	}, vkToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotSize != "4K" {
		t.Errorf("body_override size not applied: %v", gotSize)
	}
	if gotQuality != "high" {
		t.Errorf("client quality not passed through: %v", gotQuality)
	}
	if gotPrompt != "a cat" {
		t.Errorf("prompt lost: %v", gotPrompt)
	}
	if gotModel != "seedream-4" {
		t.Errorf("upstream model = %v, want physical name", gotModel)
	}
}

func TestTypedEndpointsFilterModelType(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
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
		resp := typedPost(t, h, "/v1/embeddings", map[string]any{"model": "mixed", "input": "x"}, vkToken)
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

	resp := typedPost(t, h, "/v1/embeddings", map[string]any{"model": "chatonly", "input": "x"}, vkToken)
	if resp.StatusCode != 503 {
		t.Fatalf("chat-only route on embeddings: status = %d, want 503", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "all_backends") {
		t.Errorf("want all_backends error, got %s", b)
	}
}

func TestTypedEndpointValidation(t *testing.T) {
	_, h, vkToken := newTestStackWithVK(t)
	if resp := typedPost(t, h, "/v1/embeddings", map[string]any{"input": "x"}, vkToken); resp.StatusCode != 400 {
		t.Errorf("missing model: status = %d", resp.StatusCode)
	}
	if resp := typedPost(t, h, "/v1/rerank", map[string]any{"model": "nope", "query": "q"}, vkToken); resp.StatusCode != 404 {
		t.Errorf("unknown route: status = %d", resp.StatusCode)
	}
	if resp := typedPost(t, h, "/v1/embeddings", "not-json", vkToken); resp.StatusCode != 400 {
		t.Errorf("bad json: status = %d", resp.StatusCode)
	}
}

// 编译期保证 Handler 实现 api.TypedPlane。
var _ interface {
	Embeddings(w http.ResponseWriter, r *http.Request)
	Rerank(w http.ResponseWriter, r *http.Request)
	Images(w http.ResponseWriter, r *http.Request)
} = (*proxy.Handler)(nil)
