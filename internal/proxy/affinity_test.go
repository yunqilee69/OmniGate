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

func seedTwoUpstreams(t *testing.T, st *store.Store, upA, upB *httptest.Server) {
	t.Helper()
	pA := store.Provider{Name: "zhipu", BaseURL: upA.URL}
	st.DB.Create(&pA)
	mA := store.Model{ProviderID: pA.ID, Name: "glm-4.6"}
	st.DB.Create(&mA)
	kA := store.ApiKey{ProviderID: pA.ID, KeyValue: "sk-x", Status: "active"}
	st.DB.Create(&kA)
	st.DB.Create(&store.ModelKey{ModelID: mA.ID, KeyID: kA.ID})

	pB := store.Provider{Name: "other", BaseURL: upB.URL}
	st.DB.Create(&pB)
	mB := store.Model{ProviderID: pB.ID, Name: "glm-4.5-flash"}
	st.DB.Create(&mB)
	kB := store.ApiKey{ProviderID: pB.ID, KeyValue: "sk-y", Status: "active"}
	st.DB.Create(&kB)
	st.DB.Create(&store.ModelKey{ModelID: mB.ID, KeyID: kB.ID})

	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mA.ID, Weight: 1})
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mB.ID, Weight: 1})
}

func upstreamServer(name string, hits *atomic.Int32, fail *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"from %s"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, name)
	}))
}

func postWithSession(t *testing.T, h http.Handler, body map[string]any, session string) string {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Session-ID", session)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	return string(b)
}

func newAffinityStack(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, rtm := newStackWithRTM(t)
	err := rtm.Update(map[string]json.RawMessage{"affinity.enabled": json.RawMessage("true")})
	if err != nil {
		t.Fatalf("enable affinity: %v", err)
	}
	if !rtm.Snapshot().AffinityEnabled {
		t.Fatal("affinity must be enabled after update")
	}
	return st, hWithRTM(st, rtm)
}

func TestSessionAffinityStickyByHeader(t *testing.T) {
	st, h := newAffinityStack(t)
	var hitsA, hitsB atomic.Int32
	var failA, failB atomic.Bool
	upA := upstreamServer("A", &hitsA, &failA)
	defer upA.Close()
	upB := upstreamServer("B", &hitsB, &failB)
	defer upB.Close()
	seedTwoUpstreams(t, st, upA, upB)

	body := map[string]any{"model": "glm-pool", "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	postWithSession(t, h, body, "s1")
	onA := hitsA.Load() == 1 && hitsB.Load() == 0

	for i := 0; i < 30; i++ {
		postWithSession(t, h, body, "s1")
	}
	total := hitsA.Load() + hitsB.Load()
	if total != 31 {
		t.Fatalf("expect 31 upstream hits, got %d", total)
	}
	if onA && hitsB.Load() != 0 {
		t.Fatalf("session on A must never hit B: A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
	if !onA && hitsA.Load() != 0 {
		t.Fatalf("session on B must never hit A: A=%d B=%d", hitsA.Load(), hitsB.Load())
	}

	first2 := postWithSession(t, h, body, "s2")
	for i := 0; i < 10; i++ {
		if got := postWithSession(t, h, body, "s2"); got != first2 {
			t.Fatalf("session s2 must stay sticky: %s", got)
		}
	}
}

func TestSessionAffinityStickyByMessagePrefix(t *testing.T) {
	st, h := newAffinityStack(t)
	var hitsA, hitsB atomic.Int32
	var failA, failB atomic.Bool
	upA := upstreamServer("A", &hitsA, &failA)
	defer upA.Close()
	upB := upstreamServer("B", &hitsB, &failB)
	defer upB.Close()
	seedTwoUpstreams(t, st, upA, upB)

	turn1 := map[string]any{"model": "glm-pool", "messages": []map[string]any{
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": "hi"},
	}}
	postWithSession(t, h, turn1, "")
	for i := 0; i < 20; i++ {
		turnN := map[string]any{"model": "glm-pool", "messages": []map[string]any{
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello!"},
			{"role": "user", "content": fmt.Sprintf("round %d", i)},
		}}
		postWithSession(t, h, turnN, "")
	}
	total := hitsA.Load() + hitsB.Load()
	if total != 21 {
		t.Fatalf("expect 21 upstream hits, got %d", total)
	}
	if hitsA.Load() != 0 && hitsB.Load() != 0 {
		t.Fatalf("same conversation must stay on one upstream: A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
}

func TestSessionAffinityFailoverRewrites(t *testing.T) {
	st, h := newAffinityStack(t)
	var hitsA, hitsB atomic.Int32
	var failA, failB atomic.Bool
	upA := upstreamServer("A", &hitsA, &failA)
	defer upA.Close()
	upB := upstreamServer("B", &hitsB, &failB)
	defer upB.Close()
	seedTwoUpstreams(t, st, upA, upB)

	body := map[string]any{"model": "glm-pool", "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	postWithSession(t, h, body, "s1")
	onA := hitsA.Load() == 1

	var stickyFailFlag *atomic.Bool
	var otherHits *atomic.Int32
	if onA {
		stickyFailFlag, otherHits = &failA, &hitsB
	} else {
		stickyFailFlag, otherHits = &failB, &hitsA
	}

	stickyFailFlag.Store(true)
	postWithSession(t, h, body, "s1")
	if otherHits.Load() == 0 {
		t.Fatal("failover must land on the other upstream")
	}

	afterOther := otherHits.Load()
	for i := 0; i < 10; i++ {
		postWithSession(t, h, body, "s1")
	}
	if otherHits.Load() != afterOther+10 {
		t.Fatalf("post-failover requests must all hit the new upstream: before=%d after=%d", afterOther, otherHits.Load())
	}
}

func TestSessionAffinityDisabledByDefault(t *testing.T) {
	st, rtm := newStackWithRTM(t)
	if rtm.Snapshot().AffinityEnabled {
		t.Fatal("affinity must default to off")
	}
	h := hWithRTM(st, rtm)
	var hitsA, hitsB atomic.Int32
	var failA, failB atomic.Bool
	upA := upstreamServer("A", &hitsA, &failA)
	defer upA.Close()
	upB := upstreamServer("B", &hitsB, &failB)
	defer upB.Close()
	seedTwoUpstreams(t, st, upA, upB)

	body := map[string]any{"model": "glm-pool", "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	for i := 0; i < 60; i++ {
		postWithSession(t, h, body, "s1")
	}
	if hitsA.Load() == 0 || hitsB.Load() == 0 {
		t.Fatalf("affinity off must keep weighted distribution: A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
}
