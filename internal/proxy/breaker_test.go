package proxy_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

func authKeyServer(t *testing.T, behavior map[string]int, retryAfter string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		code, known := behavior[auth]
		if !known {
			code = 200
		}
		if code == 200 {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		if code == 429 && retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":{"code":%d}}`, code)
	}))
}

func seedTwoKeys(t *testing.T, st *store.Store, url string) {
	t.Helper()
	p := store.Provider{Name: "zhipu", BaseURL: url}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-good", Status: "active"})
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-bad", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})
}

func TestKeyDisabledOn401ThroughProxy(t *testing.T) {
	st, h := newTestStack(t)
	up := authKeyServer(t, map[string]int{"Bearer sk-bad": 401}, "")
	defer up.Close()
	seedTwoKeys(t, st, up.URL)

	resp := post(t, h, chatBody(false))
	if resp.StatusCode != 200 {
		t.Fatalf("should transfer to good key, got %d", resp.StatusCode)
	}
	var bad store.ApiKey
	st.DB.Where("key_value = ?", "sk-bad").First(&bad)
	if bad.Status != "disabled" || bad.DisableReason == "" {
		t.Fatalf("401 key should be disabled: %+v", bad)
	}
	var good store.ApiKey
	st.DB.Where("key_value = ?", "sk-good").First(&good)
	if good.Status != "active" {
		t.Fatalf("good key state wrong: %+v", good)
	}
	ls := logs(t, st)
	last := ls[len(ls)-1]
	if last.Status != "success" || last.Retries != 1 {
		t.Fatalf("log wrong: %+v", last)
	}
	first := ls[0]
	if first.Status != "error" || first.ErrorCode != "401" || first.Retries != 0 {
		t.Fatalf("first failed attempt must be logged separately: %+v", first)
	}

	// sk-bad 已禁用：下一个请求直接走 sk-good，不再消耗重试
	resp = post(t, h, chatBody(false))
	if resp.StatusCode != 200 {
		t.Fatalf("second request failed: %d", resp.StatusCode)
	}
	ls = logs(t, st)
	if ls[2].Retries != 0 {
		t.Fatalf("disabled key must be skipped upfront: %+v", ls[2])
	}
}

func TestKeyCooldownOn429ThroughProxy(t *testing.T) {
	st, h := newTestStack(t)
	up := authKeyServer(t, map[string]int{"Bearer sk-bad": 429}, "7")
	defer up.Close()
	seedTwoKeys(t, st, up.URL)

	resp := post(t, h, chatBody(false))
	if resp.StatusCode != 200 {
		t.Fatalf("should transfer on 429, got %d", resp.StatusCode)
	}
	var bad store.ApiKey
	st.DB.Where("key_value = ?", "sk-bad").First(&bad)
	if bad.Status != "cooldown" || bad.RateLimitCount != 1 {
		t.Fatalf("429 key should cooldown: %+v", bad)
	}
	remain := bad.CooldownUntil - time.Now().Unix()
	if remain < 4 || remain > 8 {
		t.Fatalf("Retry-After=7 should be honored, remain=%d", remain)
	}
	var m store.Model
	st.DB.First(&m)
	if m.FailCount != 0 || m.Status != "active" {
		t.Fatalf("429 must NOT count toward model breaker: %+v", m)
	}
}

func TestModelBreakerEscalationThroughProxy(t *testing.T) {
	st, h := newTestStack(t)
	up := authKeyServer(t, map[string]int{}, "")
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":"down"}`)
	}))
	defer broken.Close()
	defer up.Close()

	pBad := store.Provider{Name: "badp", BaseURL: broken.URL}
	st.DB.Create(&pBad)
	poolBad := store.KeyPool{ProviderID: pBad.ID, Name: "pb"}
	st.DB.Create(&poolBad)
	mBad := store.Model{ProviderID: pBad.ID, Name: "badm"}
	st.DB.Create(&mBad)
	st.DB.Create(&store.ApiKey{PoolID: poolBad.ID, KeyValue: "sk-x", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: mBad.ID, PoolID: poolBad.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mBad.ID, Weight: 1})

	if resp := post(t, h, chatBody(false)); resp.StatusCode != 502 {
		t.Fatalf("single-broken-backend expected 502, got %d", resp.StatusCode)
	}
	st.DB.First(&mBad, mBad.ID)
	if mBad.FailCount != 1 || mBad.Status != "cooldown" {
		t.Fatalf("fail1 ladder: %+v", mBad)
	}
	remain := mBad.CooldownUntil - time.Now().Unix()
	if remain < 25 || remain > 31 {
		t.Fatalf("first ladder step should be ~30s, got %d", remain)
	}

	// 冷却中：无其他后端 → 503 all_backends（证明被跳过）
	if resp := post(t, h, chatBody(false)); resp.StatusCode != 503 {
		t.Fatalf("cooldown model must be skipped (503), got %d", resp.StatusCode)
	}
	ls := logs(t, st)
	if ls[1].ErrorCode != "all_backends" {
		t.Fatalf("expected all_backends, got %+v", ls[1])
	}

	// 半开探测失败 → 升级第 2 档（60s）
	st.DB.Model(&mBad).Update("cooldown_until", time.Now().Unix()-1)
	post(t, h, chatBody(false))
	st.DB.First(&mBad, mBad.ID)
	if mBad.FailCount != 2 {
		t.Fatalf("half-open probe failure should escalate: %+v", mBad)
	}
	remain = mBad.CooldownUntil - time.Now().Unix()
	if remain < 55 || remain > 62 {
		t.Fatalf("second ladder step should be ~60s, got %d", remain)
	}

	// 第 3 次失败 → 禁用 + 原因
	st.DB.Model(&mBad).Update("cooldown_until", time.Now().Unix()-1)
	post(t, h, chatBody(false))
	st.DB.First(&mBad, mBad.ID)
	if mBad.Status != "disabled" || mBad.DisableReason == "" {
		t.Fatalf("threshold=3 should disable: %+v", mBad)
	}

	// 半开成功 → 清零回归：指到好上游再探测
	st.DB.Model(&mBad).Updates(map[string]any{
		"status": "cooldown", "cooldown_until": time.Now().Unix() - 1, "fail_count": 2,
	})
	pGood := store.Provider{Name: "goodp", BaseURL: up.URL}
	st.DB.Create(&pGood)
	var goodPool store.KeyPool
	st.DB.Where("provider_id = ?", pGood.ID).First(&goodPool)
	if goodPool.ID == 0 {
		goodPool = store.KeyPool{ProviderID: pGood.ID, Name: "pg"}
		st.DB.Create(&goodPool)
	}
	st.DB.Create(&store.ApiKey{PoolID: goodPool.ID, KeyValue: "sk-y", Status: "active"})
	st.DB.Model(&mBad).Update("provider_id", pGood.ID)
	st.DB.Create(&store.ModelPool{ModelID: mBad.ID, PoolID: goodPool.ID})
	if resp := post(t, h, chatBody(false)); resp.StatusCode != 200 {
		t.Fatalf("probe request failed: %d", resp.StatusCode)
	}
	st.DB.First(&mBad, mBad.ID)
	if mBad.FailCount != 0 || mBad.Status != "active" {
		t.Fatalf("probe success should reset: %+v", mBad)
	}
}

func TestHalfOpenProbeSuccessFlow(t *testing.T) {
	st, h := newTestStack(t)
	fail := true
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"boom"}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "zhipu", BaseURL: up.URL}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-h", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	if resp := post(t, h, chatBody(false)); resp.StatusCode != 502 {
		t.Fatalf("all-fail expected 502, got %d", resp.StatusCode)
	}
	st.DB.First(&m, m.ID)
	if m.FailCount == 0 {
		t.Fatal("model failures must be recorded")
	}

	fail = false
	st.DB.Model(&m).Update("cooldown_until", time.Now().Unix()-1)
	if resp := post(t, h, chatBody(false)); resp.StatusCode != 200 {
		t.Fatalf("probe should succeed, got %d", resp.StatusCode)
	}
	st.DB.First(&m, m.ID)
	if m.FailCount != 0 || m.Status != "active" {
		t.Fatalf("successful probe must reset breaker: %+v", m)
	}
}

func TestClientErrorNoBreakerRecord(t *testing.T) {
	st, h := newTestStack(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad param"}}`)
	}))
	defer up.Close()

	p := store.Provider{Name: "zhipu", BaseURL: up.URL}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	st.DB.Create(&store.ApiKey{PoolID: pool.ID, KeyValue: "sk-n", Status: "active"})
	st.DB.Create(&store.ModelPool{ModelID: m.ID, PoolID: pool.ID})
	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m.ID, Weight: 1})

	post(t, h, chatBody(false))
	st.DB.First(&m, m.ID)
	var k store.ApiKey
	st.DB.First(&k)
	if m.FailCount != 0 || m.Status != "active" || k.Status != "active" {
		t.Fatalf("client errors must not touch breaker: m=%+v k=%+v", m, k)
	}
}
