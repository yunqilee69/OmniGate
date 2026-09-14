package proxy_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

// seedStreamRoute 建一个直通流式上游（首字前 30ms，usage 事件真实回报），路由名 glm-tps。
func seedStreamRoute(t *testing.T, st *store.Store, completionTokens int) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		time.Sleep(30 * time.Millisecond)
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"你好"}}]}`,
			`data: {"choices":[{"delta":{"content":"世界"}}]}`,
		}
		for _, s := range chunks {
			fmt.Fprintln(w, s)
			fmt.Fprintln(w)
			fl.Flush()
			time.Sleep(250 * time.Millisecond)
		}
		if completionTokens > 0 {
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":%d}}\n\n", completionTokens)
			fmt.Fprintln(w)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fmt.Fprintln(w)
		fl.Flush()
	}))
	t.Cleanup(up.Close)

	p := store.Provider{Name: "tps-prov", BaseURL: up.URL}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "tps-model"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-tps", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "glm-tps"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})
	return up
}

func streamChat(t *testing.T, h http.Handler, vkToken string) {
	t.Helper()
	body := map[string]any{
		"model":          "glm-tps",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]any{{"role": "user", "content": "hello"}},
	}
	resp := postWithAuth(t, h, body, vkToken)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status %d: %s", resp.StatusCode, string(b))
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("stream body wrong: %q", b)
	}
}

// TestStreamTPSEndToEnd 流式成功 + 真实 usage → request_log.tps、attempt.tps、
// 日聚合 tpsb 桶与 /api/stats/overview 的 avg_tps/p95_tps 全链路可查。
func TestStreamTPSEndToEnd(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	seedStreamRoute(t, st, 12)
	streamChat(t, h, vkToken)

	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	l := ls[0]
	// 生成窗口 = total - ttft ≈ 500ms，12 tok → ~24 tok/s；断言区间而非精确值
	genMs := l.TotalMs - l.TTFTMs
	if genMs < 500 {
		t.Fatalf("generation window too short: %+v", l)
	}
	if l.Tps < 10 || l.Tps > 40 {
		t.Fatalf("tps out of expected range: %v (genMs=%d, completion=%d)", l.Tps, genMs, l.CompletionTokens)
	}

	var att store.RequestAttempt
	if err := st.DB.Where("request_id = ?", l.RequestID).First(&att).Error; err != nil {
		t.Fatalf("attempt row missing: %v", err)
	}
	if att.Tps <= 0 {
		t.Fatalf("attempt tps missing: %+v", att)
	}

	var daily store.RequestLogDaily
	if err := st.DB.Where("status = ?", "success").First(&daily).Error; err != nil {
		t.Fatalf("daily row missing: %v", err)
	}
	// 24 tok/s 落在 [20,40) = 桶 4
	if daily.TpsB4 != 1 {
		t.Fatalf("daily tps bucket wrong: %+v", daily)
	}

	// 日聚合有数据 → overview 走 rollup 路径，avg_tps/p95_tps 应从桶里出来
	rec := doAPI(t, h, "/api/stats/overview", vkToken)
	ov := rec["avg_tps"].(float64)
	if ov < 10 || ov > 40 {
		t.Fatalf("overview avg_tps wrong: %v", ov)
	}
	if rec["p95_tps"].(float64) <= 0 {
		t.Fatalf("overview p95_tps missing: %v", rec["p95_tps"])
	}
}

// TestStreamTPSEstimatedZero 上游无 usage 事件 → token 估算，速度不记录（明细与日聚合都不进桶）。
func TestStreamTPSEstimatedZero(t *testing.T) {
	st, h, vkToken := newTestStackWithVK(t)
	seedStreamRoute(t, st, 0)
	streamChat(t, h, vkToken)

	ls := logs(t, st)
	if len(ls) != 1 {
		t.Fatalf("expect 1 log, got %d", len(ls))
	}
	l := ls[0]
	if !l.TokensEstimated {
		t.Fatalf("expect estimated usage: %+v", l)
	}
	if l.Tps != 0 {
		t.Fatalf("estimated stream must not record tps: %v", l.Tps)
	}
	var sum int64
	if err := st.DB.Model(&store.RequestLogDaily{}).
		Select("COALESCE(SUM(tpsb0)+SUM(tpsb1)+SUM(tpsb2)+SUM(tpsb3)+SUM(tpsb4)+SUM(tpsb5)+" +
			"SUM(tpsb6)+SUM(tpsb7)+SUM(tpsb8)+SUM(tpsb9),0)").Scan(&sum).Error; err != nil {
		t.Fatal(err)
	}
	if sum != 0 {
		t.Fatalf("no tps sample expected in daily buckets, got sum=%d", sum)
	}
}

// doAPI 以管理接口 GET 指定路径并解码 JSON 对象。
func doAPI(t *testing.T, h http.Handler, path, vkToken string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+vkToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d — %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}
