package store

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// PurgeRetentions 按保留期清理过期日志：
//   - logRetentionDays > 0：清理过期的 request_log / request_attempt，以及 day 过期的
//     request_log_daily（统计预聚合跟随同一保留期）；
//   - captureRetentionDays > 0：清理过期的 content_log（独立保留期，与请求日志无关）；
//   - 保留期为 0 表示对应表永久保留，直接跳过。
//
// 返回按表名索引的删除行数（未触发的表不出现在结果里）。
func PurgeRetentions(db *gorm.DB, logRetentionDays, captureRetentionDays int) (map[string]int64, error) {
	deleted := map[string]int64{}
	now := time.Now().Unix()

	if logRetentionDays > 0 {
		logCutoff := now - int64(logRetentionDays)*86400
		for _, table := range []string{"request_log", "request_attempt"} {
			n, err := purgeBefore(db, table, "created_at", logCutoff)
			if err != nil {
				return deleted, err
			}
			if n > 0 {
				deleted[table] = n
			}
		}
		// 日聚合表按 day（yyyymmdd）比较：保留最近 N 个自然天
		dayCutoff := DayKey(logCutoff)
		res := db.Exec("DELETE FROM request_log_daily WHERE day < ?", dayCutoff)
		if res.Error != nil {
			return deleted, fmt.Errorf("purge request_log_daily: %w", res.Error)
		}
		if res.RowsAffected > 0 {
			deleted["request_log_daily"] = res.RowsAffected
		}
	}

	if captureRetentionDays > 0 {
		captureCutoff := now - int64(captureRetentionDays)*86400
		n, err := purgeBefore(db, "content_log", "created_at", captureCutoff)
		if err != nil {
			return deleted, err
		}
		if n > 0 {
			deleted["content_log"] = n
		}
	}
	return deleted, nil
}

// ClearStats 清空全部统计数据（request_log / request_attempt / request_log_daily）。
// content_log 属于内容捕获数据而非统计事实，不在清空范围。
func ClearStats(db *gorm.DB) (map[string]int64, error) {
	cleared := map[string]int64{}
	for _, table := range []string{"request_log", "request_attempt", "request_log_daily"} {
		res := db.Exec("DELETE FROM " + table)
		if res.Error != nil {
			return cleared, fmt.Errorf("clear %s: %w", table, res.Error)
		}
		cleared[table] = res.RowsAffected
	}
	return cleared, nil
}

func purgeBefore(db *gorm.DB, table, col string, cutoff int64) (int64, error) {
	res := db.Exec("DELETE FROM "+table+" WHERE "+col+" < ?", cutoff)
	if res.Error != nil {
		return 0, fmt.Errorf("purge %s: %w", table, res.Error)
	}
	return res.RowsAffected, nil
}
