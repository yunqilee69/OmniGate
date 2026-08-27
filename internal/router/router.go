// Package router 实现三级选择：路由目标(加权) → 密钥池(加权) → key(轮询)。
package router

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/store"
)

// Attempt 是一次转发尝试的完整物理位置。
type Attempt struct {
	Model    store.Model
	Provider store.Provider
	Pool     store.KeyPool
	Key      store.ApiKey
}

// Snapshot 是请求开始时一次性读入的路由视图；请求期间不再变化（快照语义）。
type Snapshot struct {
	Route     store.Route
	Targets   []store.RouteTarget
	Models    map[int64]store.Model
	Providers map[int64]store.Provider
	Pools     map[int64][]store.KeyPool
	Keys      map[int64][]store.ApiKey
	Weights   map[int64]int
}

// Selector 无状态加权选择器；轮询游标按池全局共享。
type Selector struct {
	db *store.Store
	rr sync.Map
}

func NewSelector(db *store.Store) *Selector {
	return &Selector{db: db}
}

func (s *Selector) LoadSnapshot(routeName string) (*Snapshot, bool, error) {
	var route store.Route
	err := s.db.DB.Preload("Targets").Where("name = ?", routeName).First(&route).Error
	if err == gorm.ErrRecordNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	snap := &Snapshot{
		Route: route, Targets: route.Targets,
		Models: map[int64]store.Model{}, Providers: map[int64]store.Provider{},
		Pools: map[int64][]store.KeyPool{}, Keys: map[int64][]store.ApiKey{},
		Weights: map[int64]int{},
	}
	modelIDs := make([]int64, 0, len(route.Targets))
	for _, t := range route.Targets {
		modelIDs = append(modelIDs, t.ModelID)
		snap.Weights[t.ModelID] = t.Weight
	}
	if len(modelIDs) == 0 {
		return snap, true, nil
	}
	var models []store.Model
	if err := s.db.DB.Where("id IN ?", modelIDs).Find(&models).Error; err != nil {
		return nil, false, err
	}
	providerIDs := make([]int64, 0, len(models))
	for _, m := range models {
		snap.Models[m.ID] = m
		providerIDs = append(providerIDs, m.ProviderID)
	}
	var providers []store.Provider
	if len(providerIDs) > 0 {
		if err := s.db.DB.Where("id IN ?", providerIDs).Find(&providers).Error; err != nil {
			return nil, false, err
		}
	}
	for _, p := range providers {
		snap.Providers[p.ID] = p
	}
	var mps []store.ModelPool
	if err := s.db.DB.Where("model_id IN ?", modelIDs).Find(&mps).Error; err != nil {
		return nil, false, err
	}
	poolIDs := make([]int64, 0, len(mps))
	for _, mp := range mps {
		poolIDs = append(poolIDs, mp.PoolID)
	}
	if len(poolIDs) > 0 {
		var pools []store.KeyPool
		if err := s.db.DB.Where("id IN ?", poolIDs).Find(&pools).Error; err != nil {
			return nil, false, err
		}
		poolByID := map[int64]store.KeyPool{}
		for _, p := range pools {
			poolByID[p.ID] = p
		}
		for _, mp := range mps {
			if p, ok := poolByID[mp.PoolID]; ok {
				snap.Pools[mp.ModelID] = append(snap.Pools[mp.ModelID], p)
			}
		}
		var keys []store.ApiKey
		if err := s.db.DB.Where("pool_id IN ?", poolIDs).Order("id").Find(&keys).Error; err != nil {
			return nil, false, err
		}
		for _, k := range keys {
			snap.Keys[k.PoolID] = append(snap.Keys[k.PoolID], k)
		}
	}
	return snap, true, nil
}

// ModelAvailable 冷却到期即视为半开可用；disabled 永不可用。
func ModelAvailable(m store.Model, now time.Time) bool {
	switch m.Status {
	case "active":
		return true
	case "cooldown":
		return m.CooldownUntil <= now.Unix()
	default:
		return false
	}
}

func KeyAvailable(k store.ApiKey, now time.Time) bool {
	switch k.Status {
	case "active":
		return true
	case "cooldown":
		return k.CooldownUntil <= now.Unix()
	default:
		return false
	}
}

func availableKeys(keys []store.ApiKey, tried map[int64]bool, now time.Time) []store.ApiKey {
	out := make([]store.ApiKey, 0, len(keys))
	for _, k := range keys {
		if !tried[k.ID] && KeyAvailable(k, now) {
			out = append(out, k)
		}
	}
	return out
}

func (s *Selector) nextRR(poolID int64, n int) int {
	v, _ := s.rr.LoadOrStore(poolID, new(atomic.Uint64))
	return int(v.(*atomic.Uint64).Add(1) % uint64(n))
}

func weightedPick(weights []int) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	r := rand.IntN(total)
	for i, w := range weights {
		if r < w {
			return i
		}
		r -= w
	}
	return len(weights) - 1
}

// Pick 在排除 tried 中 key 的候选集内做三级选择；无候选返回 ok=false。
func (s *Selector) Pick(snap *Snapshot, tried map[int64]bool, now time.Time) (Attempt, bool) {
	type candModel struct {
		model store.Model
		pools []store.KeyPool
		keys  map[int64][]store.ApiKey
	}
	var cands []candModel
	var weights []int
	for _, t := range snap.Targets {
		m, ok := snap.Models[t.ModelID]
		if !ok || !ModelAvailable(m, now) {
			continue
		}
		if _, hasProvider := snap.Providers[m.ProviderID]; !hasProvider {
			continue
		}
		cm := candModel{model: m, keys: map[int64][]store.ApiKey{}}
		for _, p := range snap.Pools[m.ID] {
			if ks := availableKeys(snap.Keys[p.ID], tried, now); len(ks) > 0 {
				cm.pools = append(cm.pools, p)
				cm.keys[p.ID] = ks
			}
		}
		if len(cm.pools) == 0 {
			continue
		}
		cands = append(cands, cm)
		weights = append(weights, snap.Weights[m.ID])
	}
	if len(cands) == 0 {
		return Attempt{}, false
	}
	cm := cands[weightedPick(weights)]
	poolWeights := make([]int, len(cm.pools))
	for i, p := range cm.pools {
		poolWeights[i] = p.Weight
	}
	pool := cm.pools[weightedPick(poolWeights)]
	keys := cm.keys[pool.ID]
	key := keys[s.nextRR(pool.ID, len(keys))]
	return Attempt{Model: cm.model, Provider: snap.Providers[cm.model.ProviderID], Pool: pool, Key: key}, true
}

// LoadSnapshotByModel 为探测场景构建单模型视图（模型+提供商+绑定池与其 key）。
func (s *Selector) LoadSnapshotByModel(modelID int64) (*Snapshot, bool, error) {
	var m store.Model
	err := s.db.DB.First(&m, modelID).Error
	if err == gorm.ErrRecordNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var provider store.Provider
	if err := s.db.DB.First(&provider, m.ProviderID).Error; err != nil {
		return nil, false, err
	}
	snap := &Snapshot{
		Models:    map[int64]store.Model{m.ID: m},
		Providers: map[int64]store.Provider{provider.ID: provider},
		Pools:     map[int64][]store.KeyPool{},
		Keys:      map[int64][]store.ApiKey{},
		Weights:   map[int64]int{m.ID: 1},
		Targets:   []store.RouteTarget{{ModelID: m.ID, Weight: 1}},
	}
	var mps []store.ModelPool
	if err := s.db.DB.Where("model_id = ?", m.ID).Find(&mps).Error; err != nil {
		return nil, false, err
	}
	poolIDs := make([]int64, 0, len(mps))
	for _, mp := range mps {
		poolIDs = append(poolIDs, mp.PoolID)
	}
	if len(poolIDs) > 0 {
		var pools []store.KeyPool
		if err := s.db.DB.Where("id IN ?", poolIDs).Find(&pools).Error; err != nil {
			return nil, false, err
		}
		for _, p := range pools {
			snap.Pools[m.ID] = append(snap.Pools[m.ID], p)
		}
		var keys []store.ApiKey
		if err := s.db.DB.Where("pool_id IN ?", poolIDs).Order("id").Find(&keys).Error; err != nil {
			return nil, false, err
		}
		for _, k := range keys {
			snap.Keys[k.PoolID] = append(snap.Keys[k.PoolID], k)
		}
	}
	return snap, true, nil
}

// PickForModel 在单模型快照内选池与 key（探测用；逻辑与 Pick 的②③级一致）。
func (s *Selector) PickForModel(snap *Snapshot, now time.Time) (Attempt, bool) {
	m := snap.Targets[0].ModelID
	var poolWeights []int
	for _, p := range snap.Pools[m] {
		poolWeights = append(poolWeights, p.Weight)
	}
	if len(snap.Pools[m]) == 0 {
		return Attempt{}, false
	}
	pool := snap.Pools[m][weightedPick(poolWeights)]
	keys := availableKeys(snap.Keys[pool.ID], nil, now)
	if len(keys) == 0 {
		return Attempt{}, false
	}
	key := keys[s.nextRR(pool.ID, len(keys))]
	return Attempt{Model: snap.Models[m], Provider: snap.Providers[snap.Models[m].ProviderID], Pool: pool, Key: key}, true
}

// BackendStatus 用于 all_backends_unavailable 错误详情。
type BackendStatus struct {
	Model      string `json:"model"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	RetryAfter int64  `json:"retry_after_s,omitempty"`
}

func BackendStatuses(snap *Snapshot, now time.Time) []BackendStatus {
	out := make([]BackendStatus, 0, len(snap.Targets))
	for _, t := range snap.Targets {
		m, ok := snap.Models[t.ModelID]
		if !ok {
			continue
		}
		bs := BackendStatus{Model: m.Name, Status: m.Status}
		switch {
		case m.Status == "disabled":
			bs.Reason = m.DisableReason
		case m.Status == "cooldown" && m.CooldownUntil > now.Unix():
			bs.RetryAfter = m.CooldownUntil - now.Unix()
		default:
			if len(availableKeys(snap.KeysFor(m.ID), nil, now)) == 0 && len(snap.Pools[m.ID]) > 0 {
				bs.Status = "no_available_key"
				bs.Reason = "所有绑定密钥池内无可用 key"
			}
		}
		out = append(out, bs)
	}
	return out
}

// KeysFor 供 BackendStatuses 汇总模型全部候选 key。
func (s *Snapshot) KeysFor(modelID int64) []store.ApiKey {
	var out []store.ApiKey
	for _, p := range s.Pools[modelID] {
		out = append(out, s.Keys[p.ID]...)
	}
	return out
}
