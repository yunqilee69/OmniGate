// Package breaker 实现失败归因处置：模型级阶梯熔断 + key 级立即禁用/短冷却。
// 状态即时落库（本地 SQLite 写放大可忽略），重启不丢。
package breaker

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/store"
)

const maxErrText = 500

type Recorder struct {
	db *store.Store
}

func New(db *store.Store) *Recorder { return &Recorder{db: db} }

func clamp(s string) string {
	if len(s) > maxErrText {
		return s[:maxErrText]
	}
	return s
}

// RecordModelFailure 阶梯升级：第 N 次连续失败冷却 ladder[min(N,len)-1]，达阈值禁用。
// 冷却到期后由 router 层放行真实流量探测（半开），成功即清零。
func (rec *Recorder) RecordModelFailure(modelID int64, errCode string, rt *config.Runtime) {
	if err := rec.db.DB.Model(&store.Model{}).Where("id = ?", modelID).Updates(map[string]any{
		"fail_count": gorm.Expr("fail_count + 1"),
		"last_error": clamp(errCode),
	}).Error; err != nil {
		return
	}
	var m store.Model
	if err := rec.db.DB.First(&m, modelID).Error; err != nil {
		return
	}
	if m.FailCount >= rt.BreakerDisableThreshold {
		rec.db.DB.Model(&m).Updates(map[string]any{
			"status": "disabled", "cooldown_until": 0,
			"disable_reason": clamp(fmt.Sprintf("连续 %d 次失败（最近错误: %s）", m.FailCount, errCode)),
		})
		return
	}
	idx := m.FailCount - 1
	if idx >= len(rt.BreakerCooldownLadder) {
		idx = len(rt.BreakerCooldownLadder) - 1
	}
	rec.db.DB.Model(&m).Updates(map[string]any{
		"status":         "cooldown",
		"cooldown_until": time.Now().Add(rt.BreakerCooldownLadder[idx]).Unix(),
		"disable_reason": "",
	})
}

// RecordModelSuccess 半开探测成功（或正常成功）：计数清零回归 active。
func (rec *Recorder) RecordModelSuccess(modelID int64) {
	rec.db.DB.Model(&store.Model{}).
		Where("id = ? AND (fail_count > 0 OR status != 'active')", modelID).
		Updates(map[string]any{
			"fail_count": 0, "status": "active", "cooldown_until": 0, "disable_reason": "",
		})
}

// RecordKeyAuthFailure 401/403：密钥失效，立即禁用并留痕供 UI 告警。
func (rec *Recorder) RecordKeyAuthFailure(keyID int64, code string) {
	rec.db.DB.Model(&store.ApiKey{}).Where("id = ?", keyID).Updates(map[string]any{
		"status":         "disabled",
		"disable_reason": clamp(fmt.Sprintf("上游返回 %s，密钥疑似失效", code)),
		"last_error":     clamp(code),
	})
}

// RecordKeyRateLimited 429：短冷却，优先 Retry-After；不计入熔断。
func (rec *Recorder) RecordKeyRateLimited(keyID int64, retryAfterS, defaultS int) {
	if retryAfterS <= 0 {
		retryAfterS = defaultS
	}
	if retryAfterS > 86400 {
		retryAfterS = 86400
	}
	rec.db.DB.Model(&store.ApiKey{}).Where("id = ?", keyID).Updates(map[string]any{
		"status":             "cooldown",
		"cooldown_until":     time.Now().Unix() + int64(retryAfterS),
		"rate_limited_count": gorm.Expr("rate_limited_count + 1"),
	})
}

// RecordKeySuccess 清冷却（key 半开探测成功）并刷新使用时间。
func (rec *Recorder) RecordKeySuccess(keyID int64) {
	rec.db.DB.Model(&store.ApiKey{}).Where("id = ?", keyID).Updates(map[string]any{
		"status": "active", "cooldown_until": 0, "last_used_at": time.Now().Unix(),
	})
}

// RecordModelKeyFailure 记录模型-密钥组合失败，短暂或永久禁用该组合。
// retryable=true 时短暂禁用（阶梯冷却），retryable=false 时永久禁用。
func (rec *Recorder) RecordModelKeyFailure(modelID, keyID int64, errCode string, retryable bool, rt *config.Runtime) {
	now := time.Now()
	var ban store.ModelKeyBan
	err := rec.db.DB.Where("model_id = ? AND key_id = ?", modelID, keyID).First(&ban).Error
	
	if err == gorm.ErrRecordNotFound {
		// 创建新的禁用记录
		status := "temp_banned"
		bannedUntil := int64(0)
		failCount := 1
		
		if retryable {
			// 可重试错误：使用第一级冷却时间
			if len(rt.BreakerCooldownLadder) > 0 {
				bannedUntil = now.Add(rt.BreakerCooldownLadder[0]).Unix()
			}
		} else {
			// 不可重试错误：永久禁用
			status = "perm_banned"
		}
		
		ban = store.ModelKeyBan{
			ModelID:     modelID,
			KeyID:       keyID,
			Status:      status,
			BannedUntil: bannedUntil,
			BanReason:   clamp(errCode),
			LastError:   clamp(errCode),
			FailCount:   failCount,
		}
		rec.db.DB.Create(&ban)
	} else if err == nil {
		// 更新现有记录
		ban.FailCount++
		ban.LastError = clamp(errCode)
		
		if retryable {
			// 可重试错误：阶梯升级冷却时间
			idx := ban.FailCount - 1
			if idx >= len(rt.BreakerCooldownLadder) {
				idx = len(rt.BreakerCooldownLadder) - 1
			}
			ban.Status = "temp_banned"
			ban.BannedUntil = now.Add(rt.BreakerCooldownLadder[idx]).Unix()
			ban.BanReason = clamp(fmt.Sprintf("连续 %d 次失败（最近错误: %s）", ban.FailCount, errCode))
		} else {
			// 不可重试错误：永久禁用
			ban.Status = "perm_banned"
			ban.BannedUntil = 0
			ban.BanReason = clamp(fmt.Sprintf("不可重试错误: %s", errCode))
		}
		
		rec.db.DB.Save(&ban)
	}
}

// RecordModelKeySuccess 记录模型-密钥组合成功，清除禁用状态。
func (rec *Recorder) RecordModelKeySuccess(modelID, keyID int64) {
	rec.db.DB.Where("model_id = ? AND key_id = ?", modelID, keyID).Delete(&store.ModelKeyBan{})
}

// IsModelKeyBanned 检查模型-密钥组合是否被禁用（临时禁用检查是否过期）。
func (rec *Recorder) IsModelKeyBanned(modelID, keyID int64, now time.Time) bool {
	var ban store.ModelKeyBan
	err := rec.db.DB.Where("model_id = ? AND key_id = ?", modelID, keyID).First(&ban).Error
	if err != nil {
		return false // 未找到记录，说明未被禁用
	}
	
	if ban.Status == "perm_banned" {
		return true // 永久禁用
	}
	
	if ban.Status == "temp_banned" && ban.BannedUntil > now.Unix() {
		return true // 临时禁用且未过期
	}
	
	// 临时禁用已过期，可以删除记录
	if ban.Status == "temp_banned" && ban.BannedUntil <= now.Unix() {
		rec.db.DB.Delete(&ban)
	}
	
	return false
}

// UnbanModelKey 手动解禁模型-密钥组合。
func (rec *Recorder) UnbanModelKey(modelID, keyID int64) error {
	return rec.db.DB.Where("model_id = ? AND key_id = ?", modelID, keyID).Delete(&store.ModelKeyBan{}).Error
}
