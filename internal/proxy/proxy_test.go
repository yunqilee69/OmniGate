package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/api"
	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/proxy"
	"github.com/cloudomni/omnigate/internal/store"
)

func newTestStack(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rt, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	return st, api.New(st, rt, "", proxy.New(st, rt)).Router()
}

func chatBody(stream bool) map[string]any {
	return map[string]any{
		"model":    "glm-pool",
		"stream":   stream,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	}
}

func post(t *testing.T, h http.Handler, body any) *http.Response {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func logs(t *testing.T, st *store.Store) []store.RequestLog {
	t.Helper()
	var out []store.RequestLog
	if err := st.DB.Order("id").Find(&out).Error; err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNonStreamRoutingAndStats(t *testing.T) {
	st, h := newTestStack(t)
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hi from A"}}],"usage":{"prompt_tokens":100,"completion_tokens":50}}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(2 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hi from B"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upB.Close()

	pA := store.Provider{Name: "zhipu", BaseURL: upA.URL}
	st.DB.Create(&pA)
	poolA := store.KeyPool{ProviderID: pA.ID, Name: "main"}
	st.DB.Create(&poolA)
	mA := store.Model{ProviderID: pA.ID, Name: "glm-4.6", InputPrice: 10, OutputPrice: 20}
	st.DB.Create(&mA)
	st.DB.Create(&store.ApiKey{PoolID: poolA.ID, KeyValue: "sk-x", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: mA.ID, PoolID: poolA.ID})

	pB := store.Provider{Name: "other", BaseURL: upB.URL}
	st.DB.Create(&pB)
	poolB := store.KeyPool{ProviderID: pB.ID, Name: "main-b"}
	st.DB.Create(&poolB)
	mB := store.Model{ProviderID: pB.ID, Name: "glm-4.5-flash", InputPrice: 1, OutputPrice: 2}
	st.DB.Create(&mB)
	st.DB.Create(&store.ApiKey{PoolID: poolB.ID, KeyValue: "sk-y", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: mB.ID, PoolID: poolB.ID})

	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mA.ID, Weight: 7})
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mB.ID, Weight: 3})

	counts := map[string]int{}
	const n = 1000
	for i := 0; i < n; i++ {
		resp := post(t, h, chatBody(false))
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		switch {
		case strings.Contains(string(b), "from A"):
			counts["A"]++
		case strings.Contains(string(b), "from B"):
			counts["B"]++
		}
	}
	if counts["A"] < 640 || counts["A"] > 760 {
		t.Fatalf("7:3 split broken: %v", counts)
	}
	all := logs(t, st)
	if len(all) != n {
		t.Fatalf("expect %d log rows, got %d", n, len(all))
	}
	var sumPrompt, sumCompletion int
	var cost float64
	for _, l := range all {
		if l.Status != "success" || l.TokensEstimated {
			t.Fatalf("bad log row: %+v", l)
		}
		sumPrompt += l.PromptTokens
		sumCompletion += l.CompletionTokens
		cost += l.Cost
		if l.TTFTMs < 1 || l.TotalMs < l.TTFTMs {
			t.Fatalf("timing missing: %+v", l)
		}
		if l.Model != "glm-4.6" && l.Model != "glm-4.5-flash" {
			t.Fatalf("bad model in log: %+v", l)
		}
	}
	// 期望值: A 次数 * (100p+50c) + B 次数 * (10p+5c)
	want := float64(counts["A"])*(100*10+50*20)/1e6 + float64(counts["B"])*(10*1+5*2)/1e6
	if cost < want*0.999 || cost > want*1.001 {
		t.Fatalf("cost mismatch: got %v want %v", cost, want)
	}
	if sumPrompt == 0 || sumCompletion == 0 {
		t.Fatalf("tokens missing: %d %d", sumPrompt, sumCompletion)
	}
}

func TestStreamPassthroughAndUsage(t *testing.T) {
	st, h := newTestStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "m0" {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"wrong model"}`)
			return
		}
		if _, ok := req["stream_options"]; !ok {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"stream_options not injected"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		time.Sleep(30 * time.Millisecond)
		for _, s := range []string{
			`data: {"choices":[{"delta":{"content":"你好"}}]}`,
			`data: {"choices":[{"delta":{"content":"世界"}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":6}}`,
			`data: [DONE]`,
		} {
			fmt.Fprintln(w, s)
			fmt.Fprintln(w)
			fl.Flush()
		}
	}))
	defer up.Close()

	p := store.Provider{Name: "zhipu", BaseURL: up.URL}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	m := store.Model{ProviderID: p.ID, Name: "m0", InputPrice: 10, OutputPrice: 20}
	st.DB.Create(&m)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-s", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := post(t, h, chatBody(true))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	sb := string(b)
	if !strings.Contains(sb, "你好") || !strings.Contains(sb, "[DONE]") {
		t.Fatalf("stream body wrong: %q", sb)
	}
	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	l := ls[0]
	if !l.IsStream || l.PromptTokens != 12 || l.CompletionTokens != 6 || l.TokensEstimated {
		t.Fatalf("stream log wrong: %+v", l)
	}
	wantCost := (12*10 + 6*20) / 1e6
	if l.Cost < wantCost*0.999 || l.Cost > wantCost*1.001 {
		t.Fatalf("cost %v want %v", l.Cost, wantCost)
	}
	if l.TTFTMs < 20 || l.TotalMs < l.TTFTMs {
		t.Fatalf("timing missing: %+v", l)
	}
}

func TestStreamEstimationWithoutUsage(t *testing.T) {
	st, h := newTestStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"abcdefgh\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := store.Provider{Name: "zhipu", BaseURL: up.URL}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-e", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := post(t, h, chatBody(true))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	ls := logs(t, st)
	l := ls[0]
	if !l.TokensEstimated || l.CompletionTokens != 2 { // 8 chars / 4
		t.Fatalf("estimation wrong: %+v", l)
	}
	if l.PromptTokens != 1 { // "hello" 5 chars / 4
		t.Fatalf("prompt estimation wrong: %+v", l)
	}
}

func TestFailoverOn500(t *testing.T) {
	st, h := newTestStack(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer good.Close()

	p := store.Provider{Name: "zhipu", BaseURL: bad.URL}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	mBad := store.Model{ProviderID: p.ID, Name: "bad"}
	st.DB.Create(&mBad)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-1", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: mBad.ID, PoolID: pool.ID})

	pGood := store.Provider{Name: "good-prov", BaseURL: good.URL}
	st.DB.Create(&pGood)
	poolGood := store.KeyPool{ProviderID: pGood.ID, Name: "main-good"}
	st.DB.Create(&poolGood)
	mGood := store.Model{ProviderID: pGood.ID, Name: "good"}
	st.DB.Create(&mGood)
	st.DB.Create(&store.ApiKey{PoolID: poolGood.ID, KeyValue: "sk-2", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: mGood.ID, PoolID: poolGood.ID})

	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mBad.ID, Weight: 1})
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mGood.ID, Weight: 1})

	resp := post(t, h, chatBody(false))
	if resp.StatusCode != 200 {
		t.Fatalf("failover should succeed, got %d", resp.StatusCode)
	}
	ls := logs(t, st)
	if len(ls) != 1 || ls[0].Status != "success" {
		t.Fatalf("log wrong: %+v", ls)
	}
	if ls[0].Retries < 0 || ls[0].Retries > 1 {
		t.Fatalf("retries out of range: %+v", ls[0])
	}
	if ls[0].Model != "good" {
		t.Fatalf("should land on good model: %+v", ls[0])
	}
}

func TestAllAttemptsFail(t *testing.T) {
	st, h := newTestStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"down"}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "zhipu", BaseURL: up.URL, TimeoutMs: 5000}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	for i := 0; i < 5; i++ {
		st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: fmt.Sprintf("sk-f%d", i), Status: "active"})
	}
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := post(t, h, chatBody(false))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expect 502, got %d", resp.StatusCode)
	}
	ls := logs(t, st)
	if len(ls) != 1 || ls[0].Status != "error" || ls[0].ErrorCode != "500" {
		t.Fatalf("log wrong: %+v", ls)
	}
	if ls[0].Retries != 3 { // max_hops=3 → 4 attempts，5 个 key 足够耗尽预算
		t.Fatalf("retries should equal max_hops: %+v", ls[0])
	}
}

func TestTimeoutTransfer(t *testing.T) {
	st, h := newTestStack(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"late"}}]}`)
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"fast"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer fast.Close()

	p := store.Provider{Name: "zhipu", BaseURL: slow.URL, TimeoutMs: 80}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	mSlow := store.Model{ProviderID: p.ID, Name: "slow"}
	st.DB.Create(&mSlow)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-t", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: mSlow.ID, PoolID: pool.ID})

	p2 := store.Provider{Name: "fast", BaseURL: fast.URL, TimeoutMs: 5000}
	st.DB.Create(&p2)
	pool2 := store.KeyPool{ProviderID: p2.ID, Name: "main2"}
	st.DB.Create(&pool2)
	mFast := store.Model{ProviderID: p2.ID, Name: "fastm"}
	st.DB.Create(&mFast)
	st.DB.Create(&store.ApiKey{PoolID: pool2.ID, KeyValue: "sk-t2", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: mFast.ID, PoolID: pool2.ID})

	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mSlow.ID, Weight: 1})
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mFast.ID, Weight: 1})

	resp := post(t, h, chatBody(false))
	if resp.StatusCode != 200 {
		t.Fatalf("timeout should transfer to fast, got %d", resp.StatusCode)
	}
	ls := logs(t, st)
	if ls[0].Model != "fastm" || ls[0].Status != "success" {
		t.Fatalf("log wrong: %+v", ls[0])
	}
}

func TestAllBackendsUnavailable(t *testing.T) {
	st, h := newTestStack(t)
	p := store.Provider{Name: "zhipu", BaseURL: "http://127.0.0.1:1"}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	m := store.Model{ProviderID: p.ID, Name: "m0", Status: "disabled", DisableReason: "连续3次超时"}
	st.DB.Create(&m)
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := post(t, h, chatBody(false))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expect 503, got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var errObj map[string]any
	_ = json.Unmarshal(b, &errObj)
	detail := errObj["error"].(map[string]any)["detail"].([]any)
	d0 := detail[0].(map[string]any)
	if d0["model"] != "m0" || d0["status"] != "disabled" {
		t.Fatalf("detail wrong: %v", detail)
	}
	ls := logs(t, st)
	if ls[0].ErrorCode != "all_backends" {
		t.Fatalf("log wrong: %+v", ls[0])
	}
}

func TestClientErrorPassthrough(t *testing.T) {
	st, h := newTestStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"message":"bad param"}}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "zhipu", BaseURL: up.URL}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-c", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := post(t, h, chatBody(false))
	if resp.StatusCode != 400 {
		t.Fatalf("expect 400 passthrough, got %d", resp.StatusCode)
	}
	ls := logs(t, st)
	if ls[0].Status != "client_error" || ls[0].ErrorCode != "400" || ls[0].Retries != 0 {
		t.Fatalf("log wrong: %+v", ls[0])
	}
}

func TestUnknownRoute(t *testing.T) {
	_, h := newTestStack(t)
	body := chatBody(false)
	body["model"] = "nope"
	resp := post(t, h, body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expect 404, got %d", resp.StatusCode)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decodeJSONBody(r *http.Request, v *map[string]any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
