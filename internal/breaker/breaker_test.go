package breaker

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/store"
)

func newStack(t *testing.T) (*store.Store, *Recorder, *config.Runtime) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "breaker.db"))
	if err != nil {
		t.Fatal(err)
	}
	rtm, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatal(err)
	}
	return st, New(st), rtm.Snapshot()
}

func seedModel(t *testing.T, st *store.Store) store.Model {
	t.Helper()
	p := store.Provider{Name: "zhipu", BaseURL: "https://x"}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	return m
}

func TestLadderProgression(t *testing.T) {
	st, rec, rt := newStack(t)
	m := seedModel(t, st)

	rec.RecordModelFailure(m.ID, "timeout", rt)
	st.DB.First(&m, m.ID)
	if m.FailCount != 1 || m.Status != "cooldown" {
		t.Fatalf("fail1: %+v", m)
	}
	if remain := m.CooldownUntil - time.Now().Unix(); remain < 25 || remain > 31 {
		t.Fatalf("fail1 cooldown should be ~30s, got %d", remain)
	}

	rec.RecordModelFailure(m.ID, "500", rt)
	st.DB.First(&m, m.ID)
	if m.FailCount != 2 || m.Status != "cooldown" {
		t.Fatalf("fail2: %+v", m)
	}
	if remain := m.CooldownUntil - time.Now().Unix(); remain < 55 || remain > 61 {
		t.Fatalf("fail2 cooldown should be ~60s, got %d", remain)
	}

	rec.RecordModelFailure(m.ID, "conn", rt)
	st.DB.First(&m, m.ID)
	if m.Status != "disabled" || m.DisableReason == "" {
		t.Fatalf("fail3 should disable: %+v", m)
	}
	if m.CooldownUntil != 0 {
		t.Fatalf("disabled model must not carry cooldown: %+v", m)
	}
}

func TestSuccessResets(t *testing.T) {
	st, rec, rt := newStack(t)
	m := seedModel(t, st)

	rec.RecordModelFailure(m.ID, "timeout", rt)
	rec.RecordModelSuccess(m.ID)
	st.DB.First(&m, m.ID)
	if m.FailCount != 0 || m.Status != "active" {
		t.Fatalf("success should reset: %+v", m)
	}
}

func TestKeyDispositions(t *testing.T) {
	st, rec, rt := newStack(t)
	_ = rt
	p := store.Provider{Name: "zhipu", BaseURL: "https://x"}
	st.DB.Create(&p)
	pool := store.KeyPool{ProviderID: p.ID, Name: "main"}
	st.DB.Create(&pool)
	bad := store.ApiKey{PoolID: pool.ID, KeyValue: "sk-bad"}
	limited := store.ApiKey{PoolID: pool.ID, KeyValue: "sk-lim"}
	st.DB.Create(&bad)
	st.DB.Create(&limited)

	rec.RecordKeyAuthFailure(bad.ID, "401")
	st.DB.First(&bad, bad.ID)
	if bad.Status != "disabled" || bad.DisableReason == "" {
		t.Fatalf("401 should disable key: %+v", bad)
	}

	rec.RecordKeyRateLimited(limited.ID, 7, 60)
	st.DB.First(&limited, limited.ID)
	if limited.Status != "cooldown" || limited.RateLimitCount != 1 {
		t.Fatalf("429 should cooldown key: %+v", limited)
	}
	if remain := limited.CooldownUntil - time.Now().Unix(); remain < 5 || remain > 8 {
		t.Fatalf("Retry-After=7 should win, got %d", remain)
	}

	rec.RecordKeyRateLimited(limited.ID, 0, 60)
	st.DB.First(&limited, limited.ID)
	if remain := limited.CooldownUntil - time.Now().Unix(); remain < 55 || remain > 61 {
		t.Fatalf("default cooldown should be 60s, got %d", remain)
	}
	if limited.RateLimitCount != 2 {
		t.Fatalf("rate count should accumulate: %+v", limited)
	}

	rec.RecordKeySuccess(limited.ID)
	st.DB.First(&limited, limited.ID)
	if limited.Status != "active" || limited.CooldownUntil != 0 {
		t.Fatalf("success should clear cooldown: %+v", limited)
	}
}
