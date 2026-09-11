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

// effectiveModelStatus 基于模型×密钥组合禁用状态聚合出模型真实可达性。
// 密钥级与模型级状态机已退役：禁用粒度仅为组合（bans: keyID → ban）。
// 返回模型状态、原因、密钥分布统计，以及聚合的连续失败数/最早冷却到期/最近错误。
func effectiveModelStatus(now int64, boundKeys []store.ApiKey, bans map[int64]store.ModelKeyBan) (status, reason string, stats keyStats, failCount int, cooldownUntil int64, lastError string) {
	stats.Total = len(boundKeys)

	avail, cooling, banned := 0, 0, 0
	for _, k := range boundKeys {
		ban, ok := bans[k.ID]
		if !ok {
			avail++
			stats.Active++
			continue
		}
		if ban.FailCount > failCount {
			failCount = ban.FailCount
			lastError = ban.LastError
		}
		switch {
		case ban.Status == "perm_banned":
			banned++
			stats.Disabled++
		case ban.Status == "temp_banned" && ban.BannedUntil > now:
			cooling++
			stats.Cooldown++
			if cooldownUntil == 0 || ban.BannedUntil < cooldownUntil {
				cooldownUntil = ban.BannedUntil
			}
		default: // temp_banned 已到期 → 半开可用
			avail++
			stats.Active++
		}
	}

	switch {
	case len(boundKeys) == 0:
		status, reason = "no_key", "未绑定密钥"
	case avail > 0:
		status = "active"
	case banned == len(boundKeys):
		status, reason = "disabled", "全部密钥已禁用（组合禁用）"
	case cooling == len(boundKeys):
		status, reason = "cooldown", "全部密钥冷却中"
	default:
		status, reason = "no_key", "无活跃密钥"
	}
	return status, reason, stats, failCount, cooldownUntil, lastError
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
		status, reason, keyStats, failCount, cooldownUntil, lastError := effectiveModelStatus(now, keysByModel[m.ID], banByModel[m.ID])
		resp.Models = append(resp.Models, healthModel{
			ID: m.ID, ProviderID: m.ProviderID, Name: m.Name, Status: status,
			FailCount: failCount, CooldownUntil: cooldownUntil,
			DisableReason: reason, LastError: lastError,
			KeyStats: &keyStats,
		})
	}
	for _, k := range keys {
		resp.Keys = append(resp.Keys, healthKey{
			ID: k.ID, ProviderID: k.ProviderID, Name: k.Name, Status: "active",
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
