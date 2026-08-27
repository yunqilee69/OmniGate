package proxy_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/proxy"
	"github.com/cloudomni/omnigate/internal/store"
)

// 模拟上游 500 错误，并校验错误响应体被无条件写入 request_log
func TestErrorBodyRecordedOnUpstreamFailure(t *testing.T) {
	st, _ := newTestStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"code":"provider_down","message":"upstream is on fire"}}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "eb-prov", BaseURL: up.URL, TimeoutMs: 3000}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-eb", Status: "active"})
	m := store.Model{ProviderID: p.ID, Name: "m", Protocol: "openai"}
	st.DB.Create(&m)
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "r"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	resp := post(t, h(st), map[string]any{
		"model":    "r",
		"messages": []map[string]any{{"role": "user", "content": "x"}},
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream 500 should bubble as 502, got %d", resp.StatusCode)
	}
	var log store.RequestLog
	st.DB.Order("id DESC").First(&log)
	if log.Status != "error" || log.ErrorCode != "500" {
		t.Fatalf("error log not recorded correctly: %+v", log)
	}
	if log.ErrorBody == "" {
		t.Fatalf("error body must be recorded regardless of capture.enabled")
	}
	if want := "upstream is on fire"; !contains(log.ErrorBody, want) {
		t.Fatalf("error body should contain upstream msg %q, got: %s", want, log.ErrorBody)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func h(st *store.Store) http.Handler {
	rtm, _ := config.NewRuntimeManager(st)
	ph := proxy.New(st, rtm)
	return apiTestRouter(st, ph, rtm)
}
