// Package breaker 实现失败归因处置：模型×密钥组合级阶梯熔断 + 永久禁用。
// 状态即时落库（本地 SQLite 写放大可忽略），重启不丢。
//
// 禁用/熔断只发生在「模型+密钥」组合这一粒度上：401/403 或连续失败达阈值 → 永久禁用；
// 可重试错误 → 阶梯冷却；429 → 短冷却。组合成功即清零。模型级与密钥级状态机已退役。
package breaker

import (
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

// RecordModelKeyFailure 记录模型-密钥组合失败。
// retryable=true（超时/5xx/连接/断流）阶梯冷却，连续失败达阈值转永久禁用；
// retryable=false（401/403）直接永久禁用。
func (rec *Recorder) RecordModelKeyFailure(modelID, keyID int64, errCode string, retryable bool, rt *config.Runtime) {
	now := time.Now()
	var ban store.ModelKeyBan
	err := rec.db.DB.Where("model_id = ? AND key_id = ?", modelID, keyID).First(&ban).Error

	if err == gorm.ErrRecordNotFound {
		ban = store.ModelKeyBan{
			ModelID:   modelID,
			KeyID:     keyID,
			Status:    "temp_banned",
			LastError: clamp(errCode),
			FailCount: 1,
		}
		if retryable {
			if ban.FailCount >= rt.BreakerDisableThreshold {
				ban.Status = "perm_banned"
			} else {
				ban.BannedUntil = now.Add(ladderStep(rt, ban.FailCount)).Unix()
			}
			ban.BanReason = failReason(ban.FailCount, errCode)
		} else {
			ban.Status = "perm_banned"
			ban.BanReason = permBanReason(errCode)
		}
		rec.db.DB.Create(&ban)
		return
	}
	if err != nil {
		return
	}

	ban.FailCount++
	ban.LastError = clamp(errCode)
	if retryable {
		if ban.FailCount >= rt.BreakerDisableThreshold {
			ban.Status = "perm_banned"
			ban.BannedUntil = 0
		} else {
			ban.Status = "temp_banned"
			ban.BannedUntil = now.Add(ladderStep(rt, ban.FailCount)).Unix()
		}
		ban.BanReason = failReason(ban.FailCount, errCode)
	} else {
		ban.Status = "perm_banned"
		ban.BannedUntil = 0
		ban.BanReason = permBanReason(errCode)
	}
	rec.db.DB.Save(&ban)
}

// RecordModelKeyRateLimited 429：组合级短冷却，优先 Retry-After；不计入熔断计数。
func (rec *Recorder) RecordModelKeyRateLimited(modelID, keyID int64, retryAfterS, defaultS int) {
	if retryAfterS <= 0 {
		retryAfterS = defaultS
	}
	if retryAfterS > 86400 {
		retryAfterS = 86400
	}
	bannedUntil := time.Now().Add(time.Duration(retryAfterS) * time.Second).Unix()

	ban := store.ModelKeyBan{
		ModelID:     modelID,
		KeyID:       keyID,
		Status:      "temp_banned",
		BannedUntil: bannedUntil,
		BanReason:   "上游限流 429",
		LastError:   "429",
	}
	// 已有记录时仅刷新冷却窗口，不推进 fail_count（限流不是故障）。
	rec.db.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "model_id"}, {Name: "key_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"status": "temp_banned", "banned_until": bannedUntil,
			"ban_reason": "上游限流 429", "last_error": "429",
		}),
	}).Create(&ban)
}

// RecordModelKeySuccess 记录模型-密钥组合成功，清除禁用状态。
func (rec *Recorder) RecordModelKeySuccess(modelID, keyID int64) {
	rec.db.DB.Where("model_id = ? AND key_id = ?", modelID, keyID).Delete(&store.ModelKeyBan{})
}

// UnbanModelKey 手动解禁模型-密钥组合。
func (rec *Recorder) UnbanModelKey(modelID, keyID int64) error {
	return rec.db.DB.Where("model_id = ? AND key_id = ?", modelID, keyID).Delete(&store.ModelKeyBan{}).Error
}

// BanAllModelKeys 手动禁用模型的所有组合（perm_banned，永不过期）。
func (rec *Recorder) BanAllModelKeys(modelID int64, reason string) error {
	var mks []store.ModelKey
	if err := rec.db.DB.Where("model_id = ?", modelID).Find(&mks).Error; err != nil {
		return err
	}
	if len(mks) == 0 {
		return nil
	}
	return rec.db.DB.Transaction(func(tx *gorm.DB) error {
		for _, mk := range mks {
			ban := store.ModelKeyBan{
				ModelID: mk.ModelID, KeyID: mk.KeyID,
				Status: "perm_banned", BanReason: reason,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "model_id"}, {Name: "key_id"}},
				DoUpdates: clause.Assignments(map[string]any{
					"status": "perm_banned", "banned_until": 0,
					"ban_reason": reason, "last_error": "",
				}),
			}).Create(&ban).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// UnbanAllModelKeys 手动解禁模型的所有组合。
func (rec *Recorder) UnbanAllModelKeys(modelID int64) error {
	return rec.db.DB.Where("model_id = ?", modelID).Delete(&store.ModelKeyBan{}).Error
}

func ladderStep(rt *config.Runtime, failCount int) time.Duration {
	idx := failCount - 1
	if idx >= len(rt.BreakerCooldownLadder) {
		idx = len(rt.BreakerCooldownLadder) - 1
	}
	return rt.BreakerCooldownLadder[idx]
}

func failReason(failCount int, errCode string) string {
	return clamp(fmt.Sprintf("连续 %d 次失败（最近错误: %s）", failCount, errCode))
}

func permBanReason(errCode string) string {
	if errCode == "401" || errCode == "403" {
		return fmt.Sprintf("上游返回 %s，密钥可能失效", errCode)
	}
	return fmt.Sprintf("不可重试错误: %s", errCode)
}
