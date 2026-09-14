package proxy_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	for _, tc := range []struct{ proto, basePath string }{{"completions", ""}, {"messages", "/v1"}} {
		id := seedProbeTarget(t, st, up.URL+tc.basePath, tc.proto)
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
	id := seedProbeTarget(t, st, up.URL, "completions")
	rtm, _ := config.NewRuntimeManager(st)
	res := proxy.ProbeModel(st, rtm, id)
	if res.Ok || res.ErrCode != "500" || res.Message == "" {
		t.Fatalf("upstream 500 should be reported: %+v", res)
	}
}

func TestProbeModelNoKey(t *testing.T) {
	st, _ := newProbeStack(t)
	id := seedProbeTarget(t, st, "http://127.0.0.1:1", "completions")
	// 密钥级禁用已移除：全部可用密钥被组合禁用同样应 no_key_available
	var mk store.ModelKey
	st.DB.First(&mk)
	st.DB.Create(&store.ModelKeyBan{
		ModelID: mk.ModelID, KeyID: mk.KeyID,
		Status: "perm_banned", BanReason: "test",
	})
	rtm, _ := config.NewRuntimeManager(st)
	res := proxy.ProbeModel(st, rtm, id)
	if res.Ok || res.ErrCode != "no_key_available" {
		t.Fatalf("no available key should fail fast: %+v", res)
	}
}

func TestProbeProvider(t *testing.T) {
	st, _ := newProbeStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	seedProbeTarget(t, st, up.URL, "completions")
	var p store.Provider
	st.DB.Where("name = ?", "probe-prov-completions").First(&p)
	var k store.ApiKey
	st.DB.Where("provider_id = ?", p.ID).First(&k)
	m2 := store.Model{ProviderID: p.ID, Name: "m-second", Protocol: "completions"}
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
	t.Cleanup(func() { _ = st.Close() })
	rtm, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatal(err)
	}
	return st, rtm
}

// TestProbeModelKeys 逐密钥探测：好坏 key 并存时每个 key 独立出结果（含名称/脱敏值/当前状态）。
func TestProbeModelKeys(t *testing.T) {
	st, rtm := newProbeStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "sk-bad") {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid key"}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "pk-prov", BaseURL: up.URL, TimeoutMs: 5000}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	kGood := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-good1111", Name: "好key", Status: "active"}
	if err := st.DB.Create(&kGood).Error; err != nil {
		t.Fatal(err)
	}
	kBad := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-bad2222", Name: "坏key", Status: "active"}
	if err := st.DB.Create(&kBad).Error; err != nil {
		t.Fatal(err)
	}
	m := store.Model{ProviderID: p.ID, Name: "m-pk", Protocol: "completions"}
	if err := st.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	for _, kid := range []int64{kGood.ID, kBad.ID} {
		if err := st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: kid}).Error; err != nil {
			t.Fatal(err)
		}
	}

	res, found := proxy.ProbeModelKeys(st, rtm, m.ID)
	if !found || res.Model != "m-pk" || len(res.Keys) != 2 {
		t.Fatalf("expect 2 key results: found=%v res=%+v", found, res)
	}
	byKey := map[int64]proxy.KeyProbeResult{}
	for _, k := range res.Keys {
		byKey[k.KeyID] = k
	}
	g, b := byKey[kGood.ID], byKey[kBad.ID]
	if !g.Ok || g.PromptTokens != 3 {
		t.Fatalf("good key should pass: %+v", g)
	}
	if g.KeyName != "好key" || g.KeyMasked == "" || g.KeyStatus != "active" {
		t.Fatalf("key meta missing: %+v", g)
	}
	if b.Ok || b.ErrCode != "401" || b.Message == "" {
		t.Fatalf("bad key should fail with 401: %+v", b)
	}

	if _, found := proxy.ProbeModelKeys(st, rtm, 99999); found {
		t.Fatal("missing model should not be found")
	}
}

func seedNamedProbeTarget(t *testing.T, st *store.Store, p store.Provider) int64 {
	t.Helper()
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-probe", Status: "active"}
	if err := st.DB.Create(&k).Error; err != nil {
		t.Fatal(err)
	}
	m := store.Model{ProviderID: p.ID, Name: "m-probe", Protocol: "completions"}
	if err := st.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID}).Error; err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// TestProbeModelUsesProviderProxy 探测请求必须走提供商 ProxyURL，不能直连上游。
func TestProbeModelUsesProviderProxy(t *testing.T) {
	st, rtm := newProbeStack(t)

	upHit := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case upHit <- struct{}{}:
		default:
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	proxyHit := make(chan struct{}, 1)
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case proxyHit <- struct{}{}:
		default:
		}
		if r.Method != http.MethodConnect {
			upReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			upReq.Header = r.Header.Clone()
			resp, err := http.DefaultClient.Do(upReq)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}
		http.Error(w, "CONNECT not used for http upstream", http.StatusBadRequest)
	}))
	defer px.Close()

	mid := seedNamedProbeTarget(t, st, store.Provider{
		Name: "probe-via-proxy", BaseURL: up.URL, ProxyURL: px.URL, TimeoutMs: 5000,
	})
	res := proxy.ProbeModel(st, rtm, mid)
	if !res.Ok {
		t.Fatalf("probe via proxy should succeed: %+v", res)
	}
	select {
	case <-proxyHit:
	default:
		t.Fatal("probe request never hit provider proxy")
	}
	select {
	case <-upHit:
	default:
		t.Fatal("probe request never reached upstream through proxy")
	}
}

// TestProbeModelHonorsProviderTimeout 探测超时用提供商 timeout_ms，不再硬顶 15s。
func TestProbeModelHonorsProviderTimeout(t *testing.T) {
	st, rtm := newProbeStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"}}]}`)
	}))
	defer up.Close()

	mid := seedNamedProbeTarget(t, st, store.Provider{
		Name: "probe-timeout", BaseURL: up.URL, TimeoutMs: 100,
	})
	start := time.Now()
	res := proxy.ProbeModel(st, rtm, mid)
	elapsed := time.Since(start)
	if res.Ok {
		t.Fatalf("slow upstream should timeout: %+v", res)
	}
	if res.ErrCode != "connection_failed" {
		t.Fatalf("timeout should surface as connection_failed: %+v", res)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("probe waited %s, should honor 100ms timeout", elapsed)
	}
}
