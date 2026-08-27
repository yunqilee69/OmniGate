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

func seedProbeTarget(t *testing.T, st *store.Store, url, protocol string) int64 {
	t.Helper()
	p := store.Provider{Name: "probe-prov-" + protocol, BaseURL: url, TimeoutMs: 5000}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-probe", Status: "active"}
	st.DB.Create(&k)
	m := store.Model{ProviderID: p.ID, Name: "m-" + protocol, Protocol: protocol}
	st.DB.Create(&m)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	return m.ID
}

func TestProbeModelOK(t *testing.T) {
	st, _ := newProbeStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			fmt.Fprint(w, `{"id":"m1","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer up.Close()

	for _, tc := range []struct{ proto string }{{"openai"}, {"anthropic"}} {
		id := seedProbeTarget(t, st, up.URL, tc.proto)
		rtm, _ := config.NewRuntimeManager(st)
		res := proxy.ProbeModel(st, rtm, id)
		if !res.Ok || res.HTTPStatus != 200 || res.LatencyMs < 0 {
			t.Fatalf("%s probe should succeed: %+v", tc.proto, res)
		}
		if res.PromptTokens != 3 || res.CompletionTokens != 2 {
			t.Fatalf("%s probe usage wrong: %+v", tc.proto, res)
		}
		if res.Protocol != tc.proto || res.KeyID == 0 {
			t.Fatalf("%s probe meta wrong: %+v", tc.proto, res)
		}
	}
}

func TestProbeModelUpstreamError(t *testing.T) {
	st, _ := newProbeStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer up.Close()
	id := seedProbeTarget(t, st, up.URL, "openai")
	rtm, _ := config.NewRuntimeManager(st)
	res := proxy.ProbeModel(st, rtm, id)
	if res.Ok || res.ErrCode != "500" || res.Message == "" {
		t.Fatalf("upstream 500 should be reported: %+v", res)
	}
}

func TestProbeModelNoKey(t *testing.T) {
	st, _ := newProbeStack(t)
	id := seedProbeTarget(t, st, "http://127.0.0.1:1", "openai")
	st.DB.Model(&store.ApiKey{}).Where("1=1").Update("status", "disabled")
	rtm, _ := config.NewRuntimeManager(st)
	res := proxy.ProbeModel(st, rtm, id)
	if res.Ok || res.ErrCode != "no_key" {
		t.Fatalf("no available key should fail fast: %+v", res)
	}
}

func TestProbeProvider(t *testing.T) {
	st, _ := newProbeStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	seedProbeTarget(t, st, up.URL, "openai")
	var p store.Provider
	st.DB.Where("name = ?", "probe-prov-openai").First(&p)
	var k store.ApiKey
	st.DB.Where("provider_id = ?", p.ID).First(&k)
	m2 := store.Model{ProviderID: p.ID, Name: "m-second", Protocol: "openai"}
	st.DB.Create(&m2)
	st.DB.Create(&store.ModelKey{ModelID: m2.ID, KeyID: k.ID})
	rtm, _ := config.NewRuntimeManager(st)
	results, found := proxy.ProbeProvider(st, rtm, p.ID)
	if !found || len(results) != 2 {
		t.Fatalf("provider probe should cover all models: found=%v n=%d", found, len(results))
	}
	for _, res := range results {
		if !res.Ok {
			t.Fatalf("all models should pass: %+v", res)
		}
	}
}

func newProbeStack(t *testing.T) (*store.Store, *config.RuntimeManager) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/probe.db")
	if err != nil {
		t.Fatal(err)
	}
	rtm, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatal(err)
	}
	return st, rtm
}
