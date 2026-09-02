package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// seedCostTarget 建一个单模型单 key 路由，价格与币种由参数指定。
func seedCostTarget(t *testing.T, st *store.Store, url, routeName string, in, out float64, currency string) {
	t.Helper()
	p := store.Provider{Name: "cost-prov-" + routeName, BaseURL: url, TimeoutMs: 5000}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	m := store.Model{ProviderID: p.ID, Name: "m-" + routeName, InputPrice: in, OutputPrice: out, PriceCurrency: currency}
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
	if err := st.DB.Create(&rt).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1}).Error; err != nil {
		t.Fatal(err)
	}
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
