package router

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seed(t *testing.T, st *store.Store) (route string) {
	t.Helper()
	p := store.Provider{Name: "zhipu", BaseURL: "https://example.com"}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	mkKeys := func(prefix string, n int) []int64 {
		t.Helper()
		ids := make([]int64, 0, n)
		for i := 0; i < n; i++ {
			k := store.ApiKey{ProviderID: p.ID, KeyValue: prefix + string(rune('a'+i)), Status: "active"}
			if err := st.DB.Create(&k).Error; err != nil {
				t.Fatal(err)
			}
			ids = append(ids, k.ID)
		}
		return ids
	}
	premium := mkKeys("pk-", 3)
	basic := mkKeys("bk-", 2)

	m46 := store.Model{ProviderID: p.ID, Name: "glm-4.6", Status: "active"}
	mFlash := store.Model{ProviderID: p.ID, Name: "glm-4.5-flash", Status: "active"}
	st.DB.Create(&m46)
	st.DB.Create(&mFlash)
	for _, id := range premium {
		st.DB.Create(&store.ModelKey{ModelID: m46.ID, KeyID: id})
	}
	for _, id := range basic {
		st.DB.Create(&store.ModelKey{ModelID: mFlash.ID, KeyID: id})
	}

	rt := store.Route{Name: "glm-pool"}
	st.DB.Create(&rt)
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: m46.ID, Weight: 7})
	st.DB.Create(&store.RouteTarget{RouteID: rt.ID, ModelID: mFlash.ID, Weight: 3})
	return rt.Name
}

func TestWeightedDistribution(t *testing.T) {
	st := newStore(t)
	route := seed(t, st)
	sel := NewSelector(st)
	snap, found, err := sel.LoadSnapshot(route)
	if err != nil || !found {
		t.Fatalf("load snapshot: found=%v err=%v", found, err)
	}
	now := time.Now()
	tried := map[int64]bool{}
	counts := map[string]int{}
	const n = 10000
	for i := 0; i < n; i++ {
		att, ok := sel.Pick(snap, tried, now, 0)
		if !ok {
			t.Fatal("pick failed")
		}
		counts[att.Model.Name]++
	}
	if counts["glm-4.6"] < 6700 || counts["glm-4.6"] > 7300 {
		t.Fatalf("7:3 distribution broken: %v", counts)
	}
	if counts["glm-4.5-flash"] < 2700 || counts["glm-4.5-flash"] > 3300 {
		t.Fatalf("7:3 distribution broken: %v", counts)
	}
}

func TestRoundRobinWithinModel(t *testing.T) {
	st := newStore(t)
	route := seed(t, st)
	sel := NewSelector(st)
	snap, _, _ := sel.LoadSnapshot(route)
	now := time.Now()
	tried := map[int64]bool{}
	seen := map[int64]int{}
	for i := 0; i < 30; i++ {
		att, ok := sel.Pick(snap, tried, now, 0)
		if !ok {
			t.Fatal("pick failed")
		}
		if att.Model.Name != "glm-4.6" {
			continue
		}
		seen[att.Key.ID]++
	}
	for id, c := range seen {
		if c < 4 || c > 10 {
			t.Fatalf("round-robin uneven for key %d: %d", id, c)
		}
	}
}

func TestExhaustionAndSkip(t *testing.T) {
	st := newStore(t)
	route := seed(t, st)

	var m store.Model
	st.DB.Where("name = ?", "glm-4.6").First(&m)
	var flash store.Model
	st.DB.Where("name = ?", "glm-4.5-flash").First(&flash)

	sel := NewSelector(st)
	snap, _, _ := sel.LoadSnapshot(route)
	now := time.Now()

	tried := map[int64]bool{}
	reachedFlash := false
	for {
		att, ok := sel.Pick(snap, tried, now, 0)
		if !ok {
			break
		}
		tried[att.Key.ID] = true
		if att.Model.Name != "glm-4.6" {
			reachedFlash = true
		}
	}
	if !reachedFlash {
		t.Fatal("exhausting glm-4.6 keys must transfer to glm-4.5-flash")
	}
	if _, ok := sel.Pick(snap, tried, now, 0); ok {
		t.Fatal("all keys tried: pick must fail")
	}

	st.DB.Model(&m).Updates(map[string]any{"status": "cooldown", "cooldown_until": now.Unix() + 60})
	st.DB.Model(&flash).Updates(map[string]any{"status": "disabled", "disable_reason": "test"})
	snap2, _, _ := sel.LoadSnapshot(route)
	if _, ok := sel.Pick(snap2, map[int64]bool{}, now, 0); ok {
		t.Fatal("cooldown + disabled models must yield no candidate")
	}

	st.DB.Model(&m).Updates(map[string]any{"status": "cooldown", "cooldown_until": now.Unix() - 1})
	snap3, _, _ := sel.LoadSnapshot(route)
	if att, ok := sel.Pick(snap3, map[int64]bool{}, now, 0); !ok || att.Model.Name != "glm-4.6" {
		t.Fatal("expired cooldown (half-open) must be selectable again")
	}
}

func TestLoadSnapshotUnknownRoute(t *testing.T) {
	st := newStore(t)
	sel := NewSelector(st)
	if _, found, err := sel.LoadSnapshot("nope"); found || err != nil {
		t.Fatalf("expect not-found, got found=%v err=%v", found, err)
	}
}

func TestAffinityLifecycle(t *testing.T) {
	sel := NewSelector(newStore(t))
	now := time.Now()

	if _, ok := sel.Affinity("k", now); ok {
		t.Fatal("empty table must miss")
	}
	sel.SetAffinity("k", 42, time.Minute, now)
	if id, ok := sel.Affinity("k", now); !ok || id != 42 {
		t.Fatalf("within ttl: got id=%d ok=%v", id, ok)
	}
	if _, ok := sel.Affinity("k", now.Add(2*time.Minute)); ok {
		t.Fatal("must expire after ttl")
	}
	if _, ok := sel.Affinity("k", now); ok {
		t.Fatal("expired entry must be lazily removed")
	}

	sel.SetAffinity("k2", 7, -time.Minute, now)
	if _, ok := sel.Affinity("k2", now); ok {
		t.Fatal("non-positive ttl must not record")
	}
}

func TestAffinityOverflowResets(t *testing.T) {
	sel := NewSelector(newStore(t))
	now := time.Now()
	for i := 0; i <= affinityCap; i++ {
		sel.SetAffinity(fmt.Sprintf("k%d", i), int64(i), time.Hour, now)
	}
	sel.affMu.Lock()
	size := len(sel.aff)
	sel.affMu.Unlock()
	expectedMin := affinityCap/2 + 1
	expectedMax := affinityCap/2 + 2
	if size < expectedMin || size > expectedMax {
		t.Fatalf("overflow must evict half (oldest), expected %d-%d, got size %d", expectedMin, expectedMax, size)
	}
}

func TestPickPrefersAffinityModel(t *testing.T) {
	st := newStore(t)
	route := seed(t, st)
	sel := NewSelector(st)
	snap, _, _ := sel.LoadSnapshot(route)
	now := time.Now()

	var flash store.Model
	st.DB.Where("name = ?", "glm-4.5-flash").First(&flash)

	// 首选模型可用：每次都锁定 flash（权重 3:7 下纯随机几乎不可能连续命中）
	for i := 0; i < 50; i++ {
		att, ok := sel.Pick(snap, map[int64]bool{}, now, flash.ID)
		if !ok || att.Model.ID != flash.ID {
			t.Fatalf("preferred model must win: ok=%v model=%d", ok, att.Model.ID)
		}
	}

	// 首选模型熔断冷却：无感降级为加权随机
	st.DB.Model(&flash).Updates(map[string]any{"status": "cooldown", "cooldown_until": now.Unix() + 60})
	snap2, _, _ := sel.LoadSnapshot(route)
	att, ok := sel.Pick(snap2, map[int64]bool{}, now, flash.ID)
	if !ok || att.Model.ID == flash.ID {
		t.Fatalf("cooldown preferred must fall back: ok=%v model=%v", ok, att.Model.Name)
	}

	// 冷却到期（半开）：首选恢复锁定
	st.DB.Model(&flash).Updates(map[string]any{"status": "cooldown", "cooldown_until": now.Unix() - 1})
	snap3, _, _ := sel.LoadSnapshot(route)
	if att, ok := sel.Pick(snap3, map[int64]bool{}, now, flash.ID); !ok || att.Model.ID != flash.ID {
		t.Fatal("half-open preferred model must win again")
	}
}
