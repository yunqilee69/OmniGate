package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestRequestLogPendingDuringFlight：请求进行中 request_log 应存在 status=pending 的中间态行，
// 完成后被更新为最终状态（而不是从头到尾只有终态）。
func TestRequestLogPendingDuringFlight(t *testing.T) {
	st, rtm := newStackWithRTM(t)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // 阻塞直到测试观察到 pending 行后放行
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"slow"}}]}`))
	}))
	defer up.Close()

	p := store.Provider{Name: "slow-prov", BaseURL: up.URL}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "m"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-1", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	h := hWithRTM(st, rtm)
	done := make(chan *http.Response, 1)
	go func() {
		done <- post(t, h, chatBody(false))
	}()

	// 请求飞行期间轮询 request_log，必须能观察到 pending 中间态
	pendingSeen := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var rows []store.RequestLog
		if err := st.DB.Find(&rows).Error; err != nil {
			t.Fatalf("poll request_log: %v", err)
		}
		for _, r := range rows {
			if r.Status == "pending" {
				pendingSeen = true
			}
		}
		if pendingSeen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)

	if !pendingSeen {
		t.Fatal("expected status=pending request_log row while request is in flight")
	}
	resp := <-done
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final response should be 200, got %d", resp.StatusCode)
	}
	final := logs(t, st)
	if len(final) != 1 || final[0].Status != "success" {
		t.Fatalf("final log should be a single success row (pending updated in place), got %+v", final)
	}
}
