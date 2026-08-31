// Package router 实现请求的两级选择：路由目标模型加权随机，模型绑定密钥轮询。
package router

import (
	"math/rand/v2"
	"sort"
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
	Key      store.ApiKey
}

// Snapshot 是请求开始时一次性读入的路由视图；请求期间不再变化（快照语义）。
type Snapshot struct {
	Route     store.Route
	Targets   []store.RouteTarget
	Models    map[int64]store.Model
	Providers map[int64]store.Provider
	Keys      map[int64][]store.ApiKey // modelID → 绑定密钥
	Weights   map[int64]int
}

// Selector 加权选择器；轮询游标按模型全局共享，另维护会话亲和记忆（会话 → 上次成功模型）。
type Selector struct {
	db *store.Store
	rr sync.Map
	affMu sync.Mutex
	aff   map[string]affinityEntry
}

// affinityCap 亲和表容量上限。写满时先清过期条目，仍满则整体重置——
// 亲和是尽力而为的短期记忆，随时可由后续请求重建，不值得引入复杂淘汰结构。
const affinityCap = 1 << 16

type affinityEntry struct {
	modelID  int64
	expireAt int64
}

func NewSelector(db *store.Store) *Selector {
	return &Selector{db: db, aff: map[string]affinityEntry{}}
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
		Keys: map[int64][]store.ApiKey{}, Weights: map[int64]int{},
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
		if m.Type == "" {
			m.Type = "chat" // 迁移前的旧行没有 type 列值，按 chat 处理
		}
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
	if err := loadModelKeys(s.db.DB, snap, modelIDs); err != nil {
		return nil, false, err
	}
	return snap, true, nil
}

// loadModelKeys 载入模型 × 密钥绑定；防御性过滤掉与模型不同提供商的 key。
func loadModelKeys(db *gorm.DB, snap *Snapshot, modelIDs []int64) error {
	var mks []store.ModelKey
	if err := db.Where("model_id IN ?", modelIDs).Find(&mks).Error; err != nil {
		return err
	}
	keyIDs := make([]int64, 0, len(mks))
	for _, mk := range mks {
		keyIDs = append(keyIDs, mk.KeyID)
	}
	if len(keyIDs) == 0 {
		return nil
	}
	var keys []store.ApiKey
	if err := db.Where("id IN ?", keyIDs).Order("id").Find(&keys).Error; err != nil {
		return err
	}
	keyByID := map[int64]store.ApiKey{}
	for _, k := range keys {
		keyByID[k.ID] = k
	}
	for _, mk := range mks {
		m, ok := snap.Models[mk.ModelID]
		if !ok {
			continue
		}
		if k, ok := keyByID[mk.KeyID]; ok && k.ProviderID == m.ProviderID {
			snap.Keys[mk.ModelID] = append(snap.Keys[mk.ModelID], k)
		}
	}
	return nil
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

func (s *Selector) nextRR(modelID int64, n int) int {
	v, _ := s.rr.LoadOrStore(modelID, new(atomic.Uint64))
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

// Pick 在排除 tried 中 key 的候选集内做两级选择（模型加权 → 模型内 key 轮询），并只挑
// chat 类型后端（embeddings/rerank 模型绝不承接 chat 请求；空 type 视为 chat 兼容旧数据）。
// preferModel 为会话亲和的首选模型（0 表示无）：可用时直接锁定，不可用时无感落入加权路径。
func (s *Selector) Pick(snap *Snapshot, tried map[int64]bool, now time.Time, preferModel int64) (Attempt, bool) {
	return s.pick(snap, tried, now, preferModel, "chat")
}

// PickTyped 在两级选择上叠加模型类型过滤（embedding/rerank 端点只挑同类型后端）。
func (s *Selector) PickTyped(snap *Snapshot, tried map[int64]bool, now time.Time, wantType string) (Attempt, bool) {
	return s.pick(snap, tried, now, 0, wantType)
}

func (s *Selector) pick(snap *Snapshot, tried map[int64]bool, now time.Time, preferModel int64, wantType string) (Attempt, bool) {
	type candModel struct {
		model store.Model
		keys  []store.ApiKey
	}
	var cands []candModel
	var weights []int
	for _, t := range snap.Targets {
		m, ok := snap.Models[t.ModelID]
		if !ok || !ModelAvailable(m, now) {
			continue
		}
		mt := m.Type
		if mt == "" {
			mt = "chat" // 旧数据无 type 值，按 chat 处理
		}
		if mt != wantType {
			continue
		}
		if _, hasProvider := snap.Providers[m.ProviderID]; !hasProvider {
			continue
		}
		if ks := availableKeys(snap.Keys[m.ID], tried, now); len(ks) > 0 {
			if preferModel != 0 && m.ID == preferModel {
				key := ks[s.nextRR(m.ID, len(ks))]
				return Attempt{Model: m, Provider: snap.Providers[m.ProviderID], Key: key}, true
			}
			cands = append(cands, candModel{model: m, keys: ks})
			weights = append(weights, snap.Weights[m.ID])
		}
	}
	if len(cands) == 0 {
		return Attempt{}, false
	}
	cm := cands[weightedPick(weights)]
	key := cm.keys[s.nextRR(cm.model.ID, len(cm.keys))]
	return Attempt{Model: cm.model, Provider: snap.Providers[cm.model.ProviderID], Key: key}, true
}

// PickFallback 根据指定的 modelID 选择可用的 key，用于兜底模型。
// 检查模型是否可用（未熔断）、是否有绑定的可用 key，返回第一个可用的 Attempt。
func (s *Selector) PickFallback(modelID int64, now time.Time) (Attempt, bool) {
	if modelID == 0 {
		return Attempt{}, false
	}
	
	var model store.Model
	if err := s.db.DB.Where("id = ?", modelID).First(&model).Error; err != nil {
		return Attempt{}, false
	}
	
	if !ModelAvailable(model, now) {
		return Attempt{}, false
	}
	
	var provider store.Provider
	if err := s.db.DB.Where("id = ?", model.ProviderID).First(&provider).Error; err != nil {
		return Attempt{}, false
	}
	
	var mks []store.ModelKey
	if err := s.db.DB.Where("model_id = ?", modelID).Find(&mks).Error; err != nil {
		return Attempt{}, false
	}
	
	keyIDs := make([]int64, 0, len(mks))
	for _, mk := range mks {
		keyIDs = append(keyIDs, mk.KeyID)
	}
	
	if len(keyIDs) == 0 {
		return Attempt{}, false
	}
	
	var keys []store.ApiKey
	if err := s.db.DB.Where("id IN ? AND provider_id = ?", keyIDs, model.ProviderID).Find(&keys).Error; err != nil {
		return Attempt{}, false
	}
	
	for _, k := range keys {
		if KeyAvailable(k, now) {
			return Attempt{Model: model, Provider: provider, Key: k}, true
		}
	}
	
	return Attempt{}, false
}

// Affinity 返回会话上次成功落地的模型；过期条目惰性删除。
func (s *Selector) Affinity(key string, now time.Time) (int64, bool) {
	s.affMu.Lock()
	defer s.affMu.Unlock()
	e, ok := s.aff[key]
	if !ok {
		return 0, false
	}
	if e.expireAt <= now.Unix() {
		delete(s.aff, key)
		return 0, false
	}
	return e.modelID, true
}

// SetAffinity 记录会话 → 模型映射，刷新 TTL。
func (s *Selector) SetAffinity(key string, modelID int64, ttl time.Duration, now time.Time) {
	if ttl <= 0 {
		return
	}
	s.affMu.Lock()
	defer s.affMu.Unlock()
	if len(s.aff) >= affinityCap {
		if !s.sweepExpiredLocked(now) {
			s.evictHalfLRU(now)
		}
	}
	s.aff[key] = affinityEntry{modelID: modelID, expireAt: now.Add(ttl).Unix()}
}

func (s *Selector) evictHalfLRU(now time.Time) {
	type item struct {
		key string
		exp int64
	}
	items := make([]item, 0, len(s.aff))
	for k, e := range s.aff {
		items = append(items, item{key: k, exp: e.expireAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].exp < items[j].exp })
	
	evictCount := len(items) / 2
	for i := 0; i < evictCount; i++ {
		delete(s.aff, items[i].key)
	}
}

func (s *Selector) sweepExpiredLocked(now time.Time) bool {
	swept := false
	for k, e := range s.aff {
		if e.expireAt <= now.Unix() {
			delete(s.aff, k)
			swept = true
		}
	}
	return swept
}

// LoadSnapshotByModel 为探测场景构建单模型视图（模型+提供商+绑定密钥）。
func (s *Selector) LoadSnapshotByModel(modelID int64) (*Snapshot, bool, error) {
	var m store.Model
	err := s.db.DB.First(&m, modelID).Error
	if err == gorm.ErrRecordNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if m.Type == "" {
		m.Type = "chat" // 迁移前的旧行没有 type 列值，按 chat 处理
	}
	var provider store.Provider
	if err := s.db.DB.First(&provider, m.ProviderID).Error; err != nil {
		return nil, false, err
	}
	snap := &Snapshot{
		Models:    map[int64]store.Model{m.ID: m},
		Providers: map[int64]store.Provider{provider.ID: provider},
		Keys:      map[int64][]store.ApiKey{},
		Weights:   map[int64]int{m.ID: 1},
		Targets:   []store.RouteTarget{{ModelID: m.ID, Weight: 1}},
	}
	if err := loadModelKeys(s.db.DB, snap, []int64{m.ID}); err != nil {
		return nil, false, err
	}
	return snap, true, nil
}

// PickForModel 在单模型快照内选 key（探测用；轮询语义与 Pick 第二级一致）。
func (s *Selector) PickForModel(snap *Snapshot, now time.Time) (Attempt, bool) {
	m := snap.Targets[0].ModelID
	keys := availableKeys(snap.Keys[m], nil, now)
	if len(keys) == 0 {
		return Attempt{}, false
	}
	key := keys[s.nextRR(m, len(keys))]
	return Attempt{Model: snap.Models[m], Provider: snap.Providers[snap.Models[m].ProviderID], Key: key}, true
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
			if len(snap.Keys[m.ID]) > 0 && len(availableKeys(snap.Keys[m.ID], nil, now)) == 0 {
				bs.Status = "no_available_key"
				bs.Reason = "所有绑定密钥不可用（禁用/冷却中）"
			}
		}
		out = append(out, bs)
	}
	return out
}
