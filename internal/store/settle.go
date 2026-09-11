package store

import (
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// SettleRequest 把一次请求的收尾写入合并进单个事务：尝试明细、日志终态、
// 日聚合 UPSERT、虚拟 key 用量结算。
//
// 高并发写路径上把原先每请求 4 次以上独立提交（attempt INSERT × N、request_log
// UPDATE/INSERT、daily UPSERT、virtual_keys UPDATE）压成 1 次，显著降低 SQLite
// 写锁争用与提交开销。事务内单条语句失败只记日志、不回滚其余写入（与原逐条写入的
// 容错语义一致）；仅事务本身失败（如 SQLITE_BUSY 超时）才返回错误。
func (s *Store) SettleRequest(log *RequestLog, attempts []RequestAttempt) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if len(attempts) > 0 {
			if err := tx.Create(&attempts).Error; err != nil {
				slog.Warn("insert request_attempt failed", "err", err, "request_id", log.RequestID)
			}
		}

		if log.ID > 0 {
			// Save 为全列 UPDATE，autoCreateTime 只在 Create 生效：
			// 调用方已显式带请求开始时间，避免 pending 行的 created_at 被零值覆盖。
			// endpoint 在入口已写入 pending 行；Omit 防止零值覆盖。
			if err := tx.Omit("endpoint").Save(log).Error; err != nil {
				slog.Error("update request_log failed", "err", err, "request_id", log.RequestID)
				return nil
			}
		} else if err := tx.Create(log).Error; err != nil {
			slog.Error("write request_log failed", "err", err, "request_id", log.RequestID)
			return nil
		}

		UpsertDaily(tx, log)

		// 成功请求才结算 VK 用量（used_usd 与 total_requests 同步累加）。
		if log.VKID > 0 && log.Status == "success" {
			if err := tx.Model(&VirtualKey{}).Where("id = ?", log.VKID).Updates(map[string]any{
				"used_usd":       gorm.Expr("used_usd + ?", log.Cost),
				"total_requests": gorm.Expr("total_requests + 1"),
				"last_used_at":   time.Now().Unix(),
			}).Error; err != nil {
				slog.Warn("record vk usage failed", "err", err, "vk_id", log.VKID, "request_id", log.RequestID)
			}
		}
		return nil
	})
}
