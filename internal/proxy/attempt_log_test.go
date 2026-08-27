package proxy_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestFailedAttemptRecorded 回归：第一次 500（被熔断为可转移）→第二次成功。
// request_log 应记 retries=1；request_attempt 表应有 2 行：1 个 error + 1 个 success。
func TestFailedAttemptRecorded(t *testing.T) {
	st, _ := newTestStack(t)

	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"code":"first_fail","message":"first attempt fails"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "fa-prov", BaseURL: up.URL, TimeoutMs: 3000}
	st.DB.Create(&p)
	k1 := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-1", Status: "active"}
	k2 := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-2", Status: "active"}
	st.DB.Create(&k1)
	st.DB.Create(&k2)
	m := store.Model{ProviderID: p.ID, Name: "m", Protocol: "openai"}
	st.DB.Create(&m)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k1.ID})
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k2.ID})
	rt := store.Route{Name: "r"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := post(t, h(st), map[string]any{
		"model":    "r",
		"messages": []map[string]any{{"role": "user", "content": "x"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("retry should succeed, got %d — %s", resp.StatusCode, readAll(t, resp))
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("upstream should be called exactly twice, got %d", hits)
	}

	var log store.RequestLog
	st.DB.Order("id DESC").First(&log)
	if log.Status != "success" || log.Retries != 1 {
		t.Fatalf("final log should be success with retries=1, got %+v", log)
	}

	var attempts []store.RequestAttempt
	st.DB.Where("request_id = ?", log.RequestID).Order("attempt asc, id asc").Find(&attempts)
	if len(attempts) != 2 {
		t.Fatalf("expected exactly 2 attempt rows, got %d", len(attempts))
	}
	if attempts[0].Status != "error" || attempts[0].ErrorCode != "500" {
		t.Fatalf("attempt #0 should record the 500, got %+v", attempts[0])
	}
	if attempts[0].ErrorBody == "" {
		t.Fatalf("attempt #0 must capture upstream error body")
	}
	if attempts[1].Status != "success" {
		t.Fatalf("attempt #1 should be success, got %+v", attempts[1])
	}
	if attempts[1].LatencyMs < 0 {
		t.Fatalf("latency must be recorded, got %d", attempts[1].LatencyMs)
	}
}
