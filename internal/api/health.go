package api

import (
	"net/http"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

type keyStats struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Cooldown int `json:"cooldown"`
	Disabled int `json:"disabled"`
}

type healthModel struct {
	ID            int64     `json:"id"`
	ProviderID    int64     `json:"provider_id"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	FailCount     int       `json:"fail_count"`
	CooldownUntil int64     `json:"cooldown_until"`
	DisableReason string    `json:"disable_reason"`
	LastError     string    `json:"last_error"`
	KeyStats      *keyStats `json:"key_stats,omitempty"`
}

type healthKey struct {
	ID            int64  `json:"id"`
	ProviderID    int64  `json:"provider_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	CooldownUntil int64  `json:"cooldown_until"`
	DisableReason string `json:"disable_reason"`
}

type healthResp struct {
	Now    int64         `json:"now"`
	Models []healthModel `json:"models"`
	Keys   []healthKey   `json:"keys"`
}

// effectiveModelStatus 根据模型熔断状态 + 绑定密钥可用性计算的真实可达性。
// 设计要点：仅显示“active”是不够的——所有 key 都处于 429 限流冷却中时，
// 模型也无法响应，应明确标记 cooldown/no_key 给运维。
// 密钥级“disabled”已移除：密钥禁用粒度下沉为模型×密钥组合（bans: keyID → ban）。
func effectiveModelStatus(now int64, m store.Model, boundKeys []store.ApiKey, bans map[int64]store.ModelKeyBan) (status, reason string, stats keyStats) {
	stats.Total = len(boundKeys)

	// 先统计所有密钥状态分布（组合禁用 > 密钥自身状态）
	avail, cooling, banned := 0, 0, 0
	for _, k := range boundKeys {
		if ban, ok := bans[k.ID]; ok {
			if ban.Status == "perm_banned" || (ban.Status == "temp_banned" && ban.BannedUntil > now) {
				banned++
				stats.Disabled++
				continue
			}
		}
		switch {
		case k.Status == "active" || (k.Status == "cooldown" && k.CooldownUntil <= now):
			avail++
			stats.Active++
		case k.Status == "cooldown":
			cooling++
			stats.Cooldown++
		}
	}

	// 模型本身非活跃时直接返回模型状态
	status = m.Status
	reason = m.DisableReason
	if status != "active" {
		return
	}

	// 模型活跃，但根据密钥可用性计算实际可达性
	if len(boundKeys) == 0 {
		return "no_key", "未绑定密钥", stats
	}
	if avail > 0 {
		return "active", "", stats
	}
	switch {
	case banned == len(boundKeys):
		return "no_key", "全部密钥已禁用（组合禁用）", stats
	case cooling == len(boundKeys):
		return "cooldown", "全部密钥限流冷却中", stats
	default:
		return "no_key", "无活跃密钥", stats
	}
}

// getHealth 返回全量健康状态：模型熔断态、密钥态。模型的 status 字段会基于
// “实际可达性”重算（见 effectiveModelStatus）。
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
	var bans []store.ModelKeyBan
	if err := s.store.DB.Find(&bans).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	banByModel := map[int64]map[int64]store.ModelKeyBan{}
	for _, b := range bans {
		m := banByModel[b.ModelID]
		if m == nil {
			m = map[int64]store.ModelKeyBan{}
			banByModel[b.ModelID] = m
		}
		m[b.KeyID] = b
	}
	var mks []store.ModelKey
	if err := s.store.DB.Find(&mks).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	keysByModel := map[int64][]store.ApiKey{}
	keyByID := map[int64]store.ApiKey{}
	for _, k := range keys {
		keyByID[k.ID] = k
	}
	for _, mk := range mks {
		if k, ok := keyByID[mk.KeyID]; ok {
			keysByModel[mk.ModelID] = append(keysByModel[mk.ModelID], k)
		}
	}

	resp := healthResp{Now: now, Models: []healthModel{}, Keys: []healthKey{}}
	for _, m := range models {
		status, reason, keyStats := effectiveModelStatus(now, m, keysByModel[m.ID], banByModel[m.ID])
		resp.Models = append(resp.Models, healthModel{
			ID: m.ID, ProviderID: m.ProviderID, Name: m.Name, Status: status,
			FailCount: m.FailCount, CooldownUntil: m.CooldownUntil,
			DisableReason: reason, LastError: m.LastError,
			KeyStats: &keyStats,
		})
	}
	for _, k := range keys {
		resp.Keys = append(resp.Keys, healthKey{
			ID: k.ID, ProviderID: k.ProviderID, Name: k.Name, Status: k.Status,
			CooldownUntil: k.CooldownUntil, DisableReason: k.DisableReason,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
