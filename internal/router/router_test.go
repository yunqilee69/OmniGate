package router

import (
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
	return st
}

func seed(t *testing.T, st *store.Store) (route string) {
	t.Helper()
	p := store.Provider{Name: "zhipu", BaseURL: "https://example.com"}
	if err := st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	mkPool := func(name string, weight int) store.KeyPool {
		pool := store.KeyPool{ProviderID: p.ID, Name: name, Weight: weight}
		if err := st.DB.Create(&pool).Error; err != nil {
			t.Fatal(err)
		}
		return pool
	}
	premium := mkPool("premium", 1)
	basic := mkPool("basic", 1)
	mkKeys := func(pool store.KeyPool, prefix string, n int) {
		for i := 0; i < n; i++ {
			k := store.ApiKey{PoolID: pool.ID, KeyValue: prefix + string(rune('a'+i)), Status: "active"}
			if err := st.DB.Create(&k).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	mkKeys(premium, "pk-", 3)
	mkKeys(basic, "bk-", 2)

	m46 := store.Model{ProviderID: p.ID, Name: "glm-4.6", Status: "active"}
	mFlash := store.Model{ProviderID: p.ID, Name: "glm-4.5-flash", Status: "active"}
	st.DB.Create(&m46)
	st.DB.Create(&mFlash)
	st.DB.Create(&store.ModelPool{ModelID: m46.ID, PoolID: premium.ID})
	st.DB.Create(&store.ModelPool{ModelID: mFlash.ID, PoolID: basic.ID})

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
		att, ok := sel.Pick(snap, tried, now)
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

func TestRoundRobinWithinPool(t *testing.T) {
	st := newStore(t)
	route := seed(t, st)
	sel := NewSelector(st)
	snap, _, _ := sel.LoadSnapshot(route)
	now := time.Now()
	tried := map[int64]bool{}
	seen := map[int64]int{}
	for i := 0; i < 30; i++ {
		att, ok := sel.Pick(snap, tried, now)
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
		att, ok := sel.Pick(snap, tried, now)
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
	if _, ok := sel.Pick(snap, tried, now); ok {
		t.Fatal("all keys tried: pick must fail")
	}

	st.DB.Model(&m).Updates(map[string]any{"status": "cooldown", "cooldown_until": now.Unix() + 60})
	st.DB.Model(&flash).Updates(map[string]any{"status": "disabled", "disable_reason": "test"})
	snap2, _, _ := sel.LoadSnapshot(route)
	if _, ok := sel.Pick(snap2, map[int64]bool{}, now); ok {
		t.Fatal("cooldown + disabled models must yield no candidate")
	}

	st.DB.Model(&m).Updates(map[string]any{"status": "cooldown", "cooldown_until": now.Unix() - 1})
	snap3, _, _ := sel.LoadSnapshot(route)
	if att, ok := sel.Pick(snap3, map[int64]bool{}, now); !ok || att.Model.Name != "glm-4.6" {
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
