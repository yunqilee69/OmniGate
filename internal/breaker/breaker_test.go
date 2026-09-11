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
	t.Cleanup(func() { _ = st.Close() })
	rtm, err := config.NewRuntimeManager(st)
	if err != nil {
		t.Fatal(err)
	}
	return st, New(st), rtm.Snapshot()
}

// seedCombo 创建一个 model+key 组合，返回 (modelID, keyID)。
func seedCombo(t *testing.T, st *store.Store) (int64, int64) {
	t.Helper()
	p := store.Provider{Name: "zhipu", BaseURL: "https://x"}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	k := store.ApiKey{ProviderID: p.ID, KeyValue: "sk-0", Status: "active"}
	st.DB.Create(&k)
	st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	return m.ID, k.ID
}

func banOf(t *testing.T, st *store.Store, modelID, keyID int64) store.ModelKeyBan {
	t.Helper()
	var ban store.ModelKeyBan
	if err := st.DB.Where("model_id = ? AND key_id = ?", modelID, keyID).First(&ban).Error; err != nil {
		t.Fatalf("ban not found: %v", err)
	}
	return ban
}

func TestLadderProgression(t *testing.T) {
	st, rec, rt := newStack(t)
	mID, kID := seedCombo(t, st)

	rec.RecordModelKeyFailure(mID, kID, "timeout", true, rt)
	ban := banOf(t, st, mID, kID)
	if ban.FailCount != 1 || ban.Status != "temp_banned" {
		t.Fatalf("fail1: %+v", ban)
	}
	if remain := ban.BannedUntil - time.Now().Unix(); remain < 25 || remain > 31 {
		t.Fatalf("fail1 cooldown should be ~30s, got %d", remain)
	}

	rec.RecordModelKeyFailure(mID, kID, "500", true, rt)
	ban = banOf(t, st, mID, kID)
	if ban.FailCount != 2 || ban.Status != "temp_banned" {
		t.Fatalf("fail2: %+v", ban)
	}
	if remain := ban.BannedUntil - time.Now().Unix(); remain < 55 || remain > 61 {
		t.Fatalf("fail2 cooldown should be ~60s, got %d", remain)
	}

	rec.RecordModelKeyFailure(mID, kID, "conn", true, rt)
	ban = banOf(t, st, mID, kID)
	if ban.Status != "perm_banned" || ban.BanReason == "" {
		t.Fatalf("fail3 should perm-ban: %+v", ban)
	}
	if ban.BannedUntil != 0 {
		t.Fatalf("perm-banned combo must not carry cooldown: %+v", ban)
	}
}

func TestAuthFailurePermBans(t *testing.T) {
	st, rec, rt := newStack(t)
	mID, kID := seedCombo(t, st)

	rec.RecordModelKeyFailure(mID, kID, "401", false, rt)
	ban := banOf(t, st, mID, kID)
	if ban.Status != "perm_banned" || ban.BanReason == "" {
		t.Fatalf("401 should perm-ban immediately: %+v", ban)
	}
	if ban.FailCount != 1 {
		t.Fatalf("perm-ban still records a single failure: %+v", ban)
	}
}

func TestSuccessClearsBan(t *testing.T) {
	st, rec, rt := newStack(t)
	mID, kID := seedCombo(t, st)

	rec.RecordModelKeyFailure(mID, kID, "timeout", true, rt)
	rec.RecordModelKeySuccess(mID, kID)
	var n int64
	st.DB.Model(&store.ModelKeyBan{}).Where("model_id = ? AND key_id = ?", mID, kID).Count(&n)
	if n != 0 {
		t.Fatalf("success should delete the ban, got %d rows", n)
	}
}

func TestRateLimitedShortCooldown(t *testing.T) {
	st, rec, _ := newStack(t)
	mID, kID := seedCombo(t, st)

	rec.RecordModelKeyRateLimited(mID, kID, 7, 60)
	ban := banOf(t, st, mID, kID)
	if ban.Status != "temp_banned" {
		t.Fatalf("429 should temp-ban combo: %+v", ban)
	}
	if remain := ban.BannedUntil - time.Now().Unix(); remain < 5 || remain > 8 {
		t.Fatalf("Retry-After=7 should win, got %d", remain)
	}
	if ban.FailCount != 0 {
		t.Fatalf("429 must NOT count toward breaker: %+v", ban)
	}

	// 默认冷却
	rec.RecordModelKeyRateLimited(mID, kID, 0, 60)
	ban = banOf(t, st, mID, kID)
	if remain := ban.BannedUntil - time.Now().Unix(); remain < 55 || remain > 61 {
		t.Fatalf("default cooldown should be 60s, got %d", remain)
	}
	if ban.FailCount != 0 {
		t.Fatalf("429 still must not advance fail count: %+v", ban)
	}
}

func TestRateLimitedDoesNotResetBreakerCount(t *testing.T) {
	st, rec, rt := newStack(t)
	mID, kID := seedCombo(t, st)

	rec.RecordModelKeyFailure(mID, kID, "timeout", true, rt)
	ban := banOf(t, st, mID, kID)
	if ban.FailCount != 1 {
		t.Fatalf("fail1 should set count 1: %+v", ban)
	}

	// 429 不应推进也不应清零 fail_count
	rec.RecordModelKeyRateLimited(mID, kID, 5, 60)
	ban = banOf(t, st, mID, kID)
	if ban.FailCount != 1 {
		t.Fatalf("429 must preserve fail count: %+v", ban)
	}
}

func TestBanAllAndUnbanAll(t *testing.T) {
	st, rec, _ := newStack(t)
	p := store.Provider{Name: "zhipu", BaseURL: "https://x"}
	st.DB.Create(&p)
	m := store.Model{ProviderID: p.ID, Name: "m0"}
	st.DB.Create(&m)
	for _, v := range []string{"sk-a", "sk-b", "sk-c"} {
		k := store.ApiKey{ProviderID: p.ID, KeyValue: v, Status: "active"}
		st.DB.Create(&k)
		st.DB.Create(&store.ModelKey{ModelID: m.ID, KeyID: k.ID})
	}

	if err := rec.BanAllModelKeys(m.ID, "manual"); err != nil {
		t.Fatalf("ban all: %v", err)
	}
	var bans []store.ModelKeyBan
	st.DB.Where("model_id = ?", m.ID).Find(&bans)
	if len(bans) != 3 {
		t.Fatalf("expect 3 bans, got %d", len(bans))
	}
	for _, b := range bans {
		if b.Status != "perm_banned" || b.BanReason != "manual" {
			t.Fatalf("ban wrong: %+v", b)
		}
	}

	if err := rec.UnbanAllModelKeys(m.ID); err != nil {
		t.Fatalf("unban all: %v", err)
	}
	var n int64
	st.DB.Model(&store.ModelKeyBan{}).Where("model_id = ?", m.ID).Count(&n)
	if n != 0 {
		t.Fatalf("expect 0 bans after unban all, got %d", n)
	}
}
