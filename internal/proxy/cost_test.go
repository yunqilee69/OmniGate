package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// seedCostModel 建一个单模型单 key 的路由；价格、币种与协议由调用方给出
// （Name/ProviderID 由本函数填）。路由 endpoint 跟随模型协议，messages 模型挂 messages 端点。
func seedCostModel(t *testing.T, st *store.Store, url, routeName string, m store.Model) {
	t.Helper()
	p := store.Provider{Name: "cost-prov-" + routeName, BaseURL: url, TimeoutMs: 5000}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m.ProviderID = p.ID
	m.Name = "m-" + routeName
	if err := st.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-cost-" + routeName, Status: "active"}
	if err := st.DB.Create(&k).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID}).Error; err != nil {
		t.Fatal(err)
	}
	rt := store.Route{Name: routeName}
	if m.Protocol == "messages" {
		rt.Endpoint = "messages"
	}
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1}).Error; err != nil {
		t.Fatal(err)
	}
}

// seedCostTarget 建一个单模型单 key 路由，价格与币种由参数指定。
func seedCostTarget(t *testing.T, st *store.Store, url, routeName string, in, out float64, currency string) {
	t.Helper()
	seedCostModel(t, st, url, routeName, store.Model{InputPrice: in, OutputPrice: out, PriceCurrency: currency})
}

func usageUpstream(prompt, completion int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "pong"}}},
			"usage":   map[string]int{"prompt_tokens": prompt, "completion_tokens": completion},
		})
		_, _ = w.Write(body)
	}))
}

func postChat(t *testing.T, h http.Handler, model string, vkToken string) {
	t.Helper()
	resp := postWithAuth(t, h, map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, vkToken)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d", resp.StatusCode)
	}
}

func approxEq(a, b float64) bool {
	if b == 0 {
		return a > -1e-12 && a < 1e-12
	}
	lo, hi := b*0.999, b*1.001
	return a >= lo && a <= hi
}

// CNY 定价模型按快照汇率折算为 USD 入库：输入 7.25/输出 14.5 CNY、100/50 token
// → 原始 0.00145 CNY → 默认汇率 7.25 下 USD 成本恰为 0.0002。
func TestCostCNYConversion(t *testing.T) {
	st, rtm, vkToken := newStackWithRTMAndVK(t)
	h := hWithRTM(st, rtm)
	up := usageUpstream(100, 50)
	defer up.Close()
	seedCostTarget(t, st, up.URL, "cc", 7.25, 14.5, "CNY")

	postChat(t, h, "cc", vkToken)
	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	if !approxEq(ls[0].Cost, 0.0002) {
		t.Fatalf("CNY cost should convert to 0.0002 USD, got %v", ls[0].Cost)
	}

	// 汇率热更新后，下一笔按新汇率折算（0.00145 / 2 = 0.000725）
	if err := rtm.Update(map[string]json.RawMessage{"pricing.usd_cny": json.RawMessage("2")}); err != nil {
		t.Fatalf("update rate: %v", err)
	}
	postChat(t, h, "cc", vkToken)
	ls = logs(t, st)
	if !approxEq(ls[len(ls)-1].Cost, 0.000725) {
		t.Fatalf("cost should follow updated rate, got %v", ls[len(ls)-1].Cost)
	}
}

// USD 定价（含历史模型缺省值）不折算。
func TestCostUSDUntouched(t *testing.T) {
	st, rtm, vkToken := newStackWithRTMAndVK(t)
	h := hWithRTM(st, rtm)
	up := usageUpstream(12, 6)
	defer up.Close()
	seedCostTarget(t, st, up.URL, "cu", 10, 20, "USD")

	postChat(t, h, "cu", vkToken)
	ls := logs(t, st)
	if !approxEq(ls[0].Cost, (12*10+6*20)/1e6) {
		t.Fatalf("USD cost should stay raw, got %v", ls[0].Cost)
	}

	// 缺省币种（历史行）等价 USD
	seedCostTarget(t, st, up.URL, "cd", 10, 20, "")
	postChat(t, h, "cd", vkToken)
	ls = logs(t, st)
	if !approxEq(ls[len(ls)-1].Cost, (12*10+6*20)/1e6) {
		t.Fatalf("default currency should be USD, got %v", ls[len(ls)-1].Cost)
	}
}

// usageUpstreamCached 返回 OpenAI 形状 usage，prompt_tokens_details.cached_tokens
// 表示命中缓存的输入 token（OpenAI 系协议下 cached ⊆ prompt_tokens）。
func usageUpstreamCached(prompt, completion, cached int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "pong"}}},
			"usage": map[string]any{
				"prompt_tokens": prompt, "completion_tokens": completion,
				"prompt_tokens_details": map[string]int{"cached_tokens": cached},
			},
		})
		_, _ = w.Write(body)
	}))
}

// postAnthropic 走原生 /v1/messages 直通端点（route endpoint=messages）。
func postAnthropic(t *testing.T, h http.Handler, model, vkToken string) {
	t.Helper()
	buf, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 16,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if vkToken != "" {
		req.Header.Set("Authorization", "Bearer "+vkToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expect 200, got %d — %s", rec.Code, rec.Body.String())
	}
}

// OpenAI 系协议命中缓存：prompt_tokens 含命中量，需扣除后按 cached_price 计价。
// 输入 10 / 缓存 2 / 输出 20、prompt=1000（命中 600）、completion=100
// → (400×10 + 600×2 + 100×20) / 1e6 = 0.0072。
func TestCostCachedPrice(t *testing.T) {
	st, rtm, vkToken := newStackWithRTMAndVK(t)
	h := hWithRTM(st, rtm)
	up := usageUpstreamCached(1000, 100, 600)
	defer up.Close()
	seedCostModel(t, st, up.URL, "ck", store.Model{InputPrice: 10, CachedPrice: 2, OutputPrice: 20})

	postChat(t, h, "ck", vkToken)
	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	if ls[0].CachedTokens != 600 || ls[0].PromptTokens != 1000 {
		t.Fatalf("usage should stay as reported, got prompt=%d cached=%d", ls[0].PromptTokens, ls[0].CachedTokens)
	}
	if !approxEq(ls[0].Cost, 0.0072) {
		t.Fatalf("cached tokens should bill at cached_price, got %v", ls[0].Cost)
	}
}

// cached_price 未配置（0）或为负的历史行回退输入价：命中量仍按输入价计费，与旧行为一致。
func TestCostCachedPriceFallsBackToInput(t *testing.T) {
	st, rtm, vkToken := newStackWithRTMAndVK(t)
	h := hWithRTM(st, rtm)
	up := usageUpstreamCached(1000, 100, 600)
	defer up.Close()
	seedCostModel(t, st, up.URL, "c0", store.Model{InputPrice: 10, CachedPrice: 0, OutputPrice: 20})
	seedCostModel(t, st, up.URL, "cneg", store.Model{InputPrice: 10, CachedPrice: -1, OutputPrice: 20})
	want := (1000*10 + 100*20) / 1e6

	postChat(t, h, "c0", vkToken)
	postChat(t, h, "cneg", vkToken)
	ls := logs(t, st)
	if len(ls) != 2 {
		t.Fatalf("expect 2 logs, got %d", len(ls))
	}
	for _, l := range ls {
		if !approxEq(l.Cost, want) {
			t.Fatalf("unset cached_price should fall back to input price, got %v want %v", l.Cost, want)
		}
	}
}

// Anthropic messages 的 input_tokens 不含 cache_read（两者互斥），须按各自单价相加：
// 输入 10 / 缓存 2 / 输出 20、input=400 + cache_read=600、output=100 → 0.0072。
func TestCostAnthropicCacheRead(t *testing.T) {
	st, rtm, vkToken := newStackWithRTMAndVK(t)
	h := hWithRTM(st, rtm)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("messages upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"claude"}],"stop_reason":"end_turn","usage":{"input_tokens":400,"output_tokens":100,"cache_read_input_tokens":600}}`)
	}))
	defer up.Close()
	seedCostModel(t, st, up.URL+"/v1", "cm", store.Model{
		Protocol: "messages", InputPrice: 10, CachedPrice: 2, OutputPrice: 20,
	})

	postAnthropic(t, h, "cm", vkToken)
	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	if ls[0].CachedTokens != 600 {
		t.Fatalf("cache_read_input_tokens should be recorded, got %d", ls[0].CachedTokens)
	}
	if !approxEq(ls[0].Cost, 0.0072) {
		t.Fatalf("anthropic cache read should bill at cached_price, got %v", ls[0].Cost)
	}
}

// 按次计费：成功调用记 per_call_price，与 token 用量无关。
func TestCostPerCall(t *testing.T) {
	st, rtm, vkToken := newStackWithRTMAndVK(t)
	h := hWithRTM(st, rtm)
	up := usageUpstream(1000, 500)
	defer up.Close()
	seedCostModel(t, st, up.URL, "pc", store.Model{
		BillingMode: "per_call", PerCallPrice: 0.02, InputPrice: 10, OutputPrice: 20,
	})

	postChat(t, h, "pc", vkToken)
	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	if !approxEq(ls[0].Cost, 0.02) {
		t.Fatalf("per-call cost should be 0.02, got %v", ls[0].Cost)
	}
}

// 按次 + CNY：成功调用按汇率折成 USD 入库。
func TestCostPerCallCNY(t *testing.T) {
	st, rtm, vkToken := newStackWithRTMAndVK(t)
	h := hWithRTM(st, rtm)
	up := usageUpstream(10, 10)
	defer up.Close()
	seedCostModel(t, st, up.URL, "pccny", store.Model{
		BillingMode: "per_call", PerCallPrice: 7.25, PriceCurrency: "CNY",
	})

	postChat(t, h, "pccny", vkToken)
	ls := logs(t, st)
	if !approxEq(ls[0].Cost, 1) {
		t.Fatalf("CNY per-call 7.25 should convert to 1 USD, got %v", ls[0].Cost)
	}
}

// 按次计费失败请求不计费。
func TestCostPerCallErrorIsZero(t *testing.T) {
	st, rtm, vkToken := newStackWithRTMAndVK(t)
	h := hWithRTM(st, rtm)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer up.Close()
	seedCostModel(t, st, up.URL, "pcerr", store.Model{
		BillingMode: "per_call", PerCallPrice: 0.5,
	})

	buf, _ := json.Marshal(map[string]any{
		"model": "pcerr", "messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	if ls[0].Status == "success" {
		t.Fatalf("expect error status, got success")
	}
	if ls[0].Cost != 0 {
		t.Fatalf("failed per-call request should cost 0, got %v", ls[0].Cost)
	}
}
