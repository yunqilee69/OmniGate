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
