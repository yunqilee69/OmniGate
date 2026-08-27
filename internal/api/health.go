package api

import (
	"net/http"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

type healthModel struct {
	ID            int64  `json:"id"`
	ProviderID    int64  `json:"provider_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	FailCount     int    `json:"fail_count"`
	CooldownUntil int64  `json:"cooldown_until"`
	DisableReason string `json:"disable_reason"`
	LastError     string `json:"last_error"`
}

type healthKey struct {
	ID            int64  `json:"id"`
	PoolID        int64  `json:"pool_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	CooldownUntil int64  `json:"cooldown_until"`
	DisableReason string `json:"disable_reason"`
}

type healthPool struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	ProviderID    int64  `json:"provider_id"`
	TotalKeys     int    `json:"total_keys"`
	AvailableKeys int    `json:"available_keys"`
}

type healthResp struct {
	Now    int64         `json:"now"`
	Models []healthModel `json:"models"`
	Keys   []healthKey   `json:"keys"`
	Pools  []healthPool  `json:"pools"`
}

// effectiveModelStatus 根据模型熔断状态 + 绑定密钥池内可用 key 计算的真实可达性。
// 设计要点：仅显示“active”是不够的——所有 key 都处于 429 限流冷却中时，
// 模型也无法响应，应明确标记 cooldown/disabled 给运维。
func effectiveModelStatus(now int64, m store.Model, boundKeys []store.ApiKey) (status, reason string) {
	status = m.Status
	reason = m.DisableReason
	if status != "active" {
		return
	}
	if len(boundKeys) == 0 {
		return "no_key", "未绑定密钥池"
	}
	avail, cooling, disabled := 0, 0, 0
	for _, k := range boundKeys {
		switch {
		case k.Status == "active" || (k.Status == "cooldown" && k.CooldownUntil <= now):
			avail++
		case k.Status == "cooldown":
			cooling++
		default:
			disabled++
		}
	}
	if avail > 0 {
		return "active", ""
	}
	switch {
	case disabled == len(boundKeys):
		return "no_key", "全部密钥已禁用"
	case cooling == len(boundKeys):
		return "cooldown", "全部密钥限流冷却中"
	default:
		return "no_key", "无活跃密钥"
	}
}

// getHealth 返回全量健康状态：模型熔断态、密钥态、池派生态（可用 key 计数）。
// 模型的 status 字段会基于“实际可达性”重算（见 effectiveModelStatus）。
func (s *Server) getHealth(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()

	var models []store.Model
	if err := s.store.DB.Order("id").Find(&models).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var keys []store.ApiKey
	if err := s.store.DB.Order("id").Find(&keys).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var pools []store.KeyPool
	if err := s.store.DB.Order("id").Find(&pools).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	var mps []store.ModelPool
	if err := s.store.DB.Find(&mps).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	keysByPool := map[int64][]store.ApiKey{}
	for _, k := range keys {
		keysByPool[k.PoolID] = append(keysByPool[k.PoolID], k)
	}
	keysByModel := map[int64][]store.ApiKey{}
	for _, mp := range mps {
		keysByModel[mp.ModelID] = append(keysByModel[mp.ModelID], keysByPool[mp.PoolID]...)
	}

	resp := healthResp{Now: now, Models: []healthModel{}, Keys: []healthKey{}, Pools: []healthPool{}}
	for _, m := range models {
		status, reason := effectiveModelStatus(now, m, keysByModel[m.ID])
		resp.Models = append(resp.Models, healthModel{
			ID: m.ID, ProviderID: m.ProviderID, Name: m.Name, Status: status,
			FailCount: m.FailCount, CooldownUntil: m.CooldownUntil,
			DisableReason: reason, LastError: m.LastError,
		})
	}
	for _, k := range keys {
		resp.Keys = append(resp.Keys, healthKey{
			ID: k.ID, PoolID: k.PoolID, Name: k.Name, Status: k.Status,
			CooldownUntil: k.CooldownUntil, DisableReason: k.DisableReason,
		})
	}
	availByPool := map[int64]int{}
	for _, k := range keys {
		if k.Status == "active" || (k.Status == "cooldown" && k.CooldownUntil <= now) {
			availByPool[k.PoolID]++
		}
	}
	totalByPool := map[int64]int{}
	for _, k := range keys {
		totalByPool[k.PoolID]++
	}
	for _, p := range pools {
		resp.Pools = append(resp.Pools, healthPool{
			ID: p.ID, Name: p.Name, ProviderID: p.ProviderID,
			TotalKeys: totalByPool[p.ID], AvailableKeys: availByPool[p.ID],
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
