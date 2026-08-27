package store

import (
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// 直方桶边界（ms）。桶 i (i=0..8) 装 [bounds[i-1], bounds[i])（首桶从 0 起），
// 桶 9 是开区间 [bounds[8], +∞)。p95 反查返回桶的上界（开区间桶返回 bounds[8]*2）。
var (
	TTFTBucketBounds = [9]int64{50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000}
	TotalBucketBounds = [9]int64{100, 300, 1000, 3000, 10000, 30000, 60000, 120000, 300000}
)

func ttftBucketIdx(ms int64) int {
	for i, b := range TTFTBucketBounds {
		if ms < b {
			return i
		}
	}
	return 9
}

func totalBucketIdx(ms int64) int {
	for i, b := range TotalBucketBounds {
		if ms < b {
			return i
		}
	}
	return 9
}

// DayKey 把 unix 秒转成 yyyymmdd（UTC+8，与 dayjs 默认一致）。
func DayKey(unixSec int64) int64 {
	t := time.Unix(unixSec, 0).Local()
	return int64(t.Year()*10000 + int(t.Month())*100 + t.Day())
}

// UpsertDaily 把一条 request_log 同步 UPSERT 进 request_log_daily。
// 与 Create(&entry) 串行调用，失败仅记日志，不影响主流程。
func UpsertDaily(db *gorm.DB, log *RequestLog) {
	day := DayKey(log.CreatedAt)
	if log.CreatedAt == 0 {
		day = DayKey(time.Now().Unix())
	}
	status := log.Status
	ti := ttftBucketIdx(log.TTFTMs)
	tb := totalBucketIdx(log.TotalMs)
	successDelta := int64(0)
	errorDelta := int64(0)
	if status == "success" {
		successDelta = 1
	} else {
		errorDelta = 1
	}

	now := time.Now().Unix()
	err := db.Exec(`
INSERT INTO request_log_daily
  (day, route, model, provider, pool, status,
   total, success, errors, prompt_tokens, completion_tokens, cost, retries_sum,
   ttftb0, ttftb1, ttftb2, ttftb3, ttftb4, ttftb5, ttftb6, ttftb7, ttftb8, ttftb9,
   totalb0, totalb1, totalb2, totalb3, totalb4, totalb5, totalb6, totalb7, totalb8, totalb9,
   updated_at)
VALUES (?,?,?,?,?,?,
        1,?,?,?,?,?,?,
        ?,?,?,?,?,?,?,?,?,?,
        ?,?,?,?,?,?,?,?,?,?,
        ?)
ON CONFLICT(day, route, model, provider, pool, status) DO UPDATE SET
  total            = total            + 1,
  success          = success          + excluded.success,
  errors           = errors           + excluded.errors,
  prompt_tokens    = prompt_tokens    + excluded.prompt_tokens,
  completion_tokens= completion_tokens+ excluded.completion_tokens,
  cost             = cost             + excluded.cost,
  retries_sum      = retries_sum      + excluded.retries_sum,
  ttftb0 = ttftb0 + excluded.ttftb0, ttftb1 = ttftb1 + excluded.ttftb1,
  ttftb2 = ttftb2 + excluded.ttftb2, ttftb3 = ttftb3 + excluded.ttftb3,
  ttftb4 = ttftb4 + excluded.ttftb4, ttftb5 = ttftb5 + excluded.ttftb5,
  ttftb6 = ttftb6 + excluded.ttftb6, ttftb7 = ttftb7 + excluded.ttftb7,
  ttftb8 = ttftb8 + excluded.ttftb8, ttftb9 = ttftb9 + excluded.ttftb9,
  totalb0 = totalb0 + excluded.totalb0, totalb1 = totalb1 + excluded.totalb1,
  totalb2 = totalb2 + excluded.totalb2, totalb3 = totalb3 + excluded.totalb3,
  totalb4 = totalb4 + excluded.totalb4, totalb5 = totalb5 + excluded.totalb5,
  totalb6 = totalb6 + excluded.totalb6, totalb7 = totalb7 + excluded.totalb7,
  totalb8 = totalb8 + excluded.totalb8, totalb9 = totalb9 + excluded.totalb9,
  updated_at       = excluded.updated_at
`,
		day, log.Route, log.Model, log.Provider, log.Pool, status,
		successDelta, errorDelta, log.PromptTokens, log.CompletionTokens, log.Cost, log.Retries,
		boolToInt64(ti == 0), boolToInt64(ti == 1), boolToInt64(ti == 2), boolToInt64(ti == 3),
		boolToInt64(ti == 4), boolToInt64(ti == 5), boolToInt64(ti == 6), boolToInt64(ti == 7),
		boolToInt64(ti == 8), boolToInt64(ti == 9),
		boolToInt64(tb == 0), boolToInt64(tb == 1), boolToInt64(tb == 2), boolToInt64(tb == 3),
		boolToInt64(tb == 4), boolToInt64(tb == 5), boolToInt64(tb == 6), boolToInt64(tb == 7),
		boolToInt64(tb == 8), boolToInt64(tb == 9),
		now,
	).Error
	if err != nil {
		slog.Warn("upsert request_log_daily failed", "err", err, "request_id", log.RequestID)
	}
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Backfill 用一次 GROUP BY 把现有 request_log 全部回填进 request_log_daily。
// 设计上仅在启动时调用一次：实时路径（UpsertDaily）负责维护当天及之后的数据。
// 安全可重入：INSERT 会覆盖已存在行（同 PK 全量重算）。
func Backfill(db *gorm.DB) error {
	now := time.Now().Unix()
	rows, err := db.Raw(`
SELECT
  (CAST(strftime('%Y', created_at, 'unixepoch') AS INTEGER)) * 10000
+ (CAST(strftime('%m', created_at, 'unixepoch') AS INTEGER)) * 100
+ (CAST(strftime('%d', created_at, 'unixepoch') AS INTEGER))         AS day,
  route, model, provider, COALESCE(pool,'') AS pool, status,
  COUNT(*) AS total,
  SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) AS success,
  SUM(CASE WHEN status='error'   THEN 1 ELSE 0 END) AS errors,
  COALESCE(SUM(prompt_tokens),0)     AS p_tok,
  COALESCE(SUM(completion_tokens),0) AS c_tok,
  COALESCE(SUM(cost),0)              AS cost,
  COALESCE(SUM(retries),0)           AS retries_sum
FROM request_log
GROUP BY day, route, model, provider, pool, status`).Rows()
	if err != nil {
		return err
	}
	type agg struct {
		Day              int64
		Route, Model, Provider, Pool, Status string
		Total, Success, Errors, PTok, CTok, RetriesSum int64
		Cost             float64
	}
	defer rows.Close()
	var aggs []agg
	for rows.Next() {
		var a agg
		if err := rows.Scan(&a.Day, &a.Route, &a.Model, &a.Provider, &a.Pool, &a.Status,
			&a.Total, &a.Success, &a.Errors, &a.PTok, &a.CTok, &a.Cost, &a.RetriesSum); err != nil {
			return err
		}
		aggs = append(aggs, a)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	// 第二遍：把每行的 ttft/total 直方桶计算出来（同 SQL 用 SUM(CASE) 一次性生成）。
	histRows, err := db.Raw(`
SELECT
  (CAST(strftime('%Y', created_at, 'unixepoch') AS INTEGER)) * 10000
+ (CAST(strftime('%m', created_at, 'unixepoch') AS INTEGER)) * 100
+ (CAST(strftime('%d', created_at, 'unixepoch') AS INTEGER))         AS day,
  route, model, provider, COALESCE(pool,'') AS pool, status,
  SUM(CASE WHEN ttft_ms < 50                       THEN 1 ELSE 0 END) AS t0,
  SUM(CASE WHEN ttft_ms >= 50     AND ttft_ms < 100     THEN 1 ELSE 0 END) AS t1,
  SUM(CASE WHEN ttft_ms >= 100    AND ttft_ms < 200     THEN 1 ELSE 0 END) AS t2,
  SUM(CASE WHEN ttft_ms >= 200    AND ttft_ms < 500     THEN 1 ELSE 0 END) AS t3,
  SUM(CASE WHEN ttft_ms >= 500    AND ttft_ms < 1000    THEN 1 ELSE 0 END) AS t4,
  SUM(CASE WHEN ttft_ms >= 1000   AND ttft_ms < 2000    THEN 1 ELSE 0 END) AS t5,
  SUM(CASE WHEN ttft_ms >= 2000   AND ttft_ms < 5000    THEN 1 ELSE 0 END) AS t6,
  SUM(CASE WHEN ttft_ms >= 5000   AND ttft_ms < 10000   THEN 1 ELSE 0 END) AS t7,
  SUM(CASE WHEN ttft_ms >= 10000  AND ttft_ms < 30000   THEN 1 ELSE 0 END) AS t8,
  SUM(CASE WHEN ttft_ms >= 30000                        THEN 1 ELSE 0 END) AS t9,
  SUM(CASE WHEN total_ms < 100                        THEN 1 ELSE 0 END) AS T0,
  SUM(CASE WHEN total_ms >= 100    AND total_ms < 300    THEN 1 ELSE 0 END) AS T1,
  SUM(CASE WHEN total_ms >= 300    AND total_ms < 1000   THEN 1 ELSE 0 END) AS T2,
  SUM(CASE WHEN total_ms >= 1000   AND total_ms < 3000   THEN 1 ELSE 0 END) AS T3,
  SUM(CASE WHEN total_ms >= 3000   AND total_ms < 10000  THEN 1 ELSE 0 END) AS T4,
  SUM(CASE WHEN total_ms >= 10000  AND total_ms < 30000  THEN 1 ELSE 0 END) AS T5,
  SUM(CASE WHEN total_ms >= 30000  AND total_ms < 60000  THEN 1 ELSE 0 END) AS T6,
  SUM(CASE WHEN total_ms >= 60000  AND total_ms < 120000 THEN 1 ELSE 0 END) AS T7,
  SUM(CASE WHEN total_ms >= 120000 AND total_ms < 300000 THEN 1 ELSE 0 END) AS T8,
  SUM(CASE WHEN total_ms >= 300000                       THEN 1 ELSE 0 END) AS T9
FROM request_log
GROUP BY day, route, model, provider, pool, status`).Rows()
	if err != nil {
		return err
	}
	type histKey struct{ Day int64; Route, Model, Provider, Pool, Status string }
	hists := map[histKey][20]int64{}
	defer histRows.Close()
	for histRows.Next() {
		var k histKey
		var h [20]int64
		if err := histRows.Scan(&k.Day, &k.Route, &k.Model, &k.Provider, &k.Pool, &k.Status,
			&h[0], &h[1], &h[2], &h[3], &h[4], &h[5], &h[6], &h[7], &h[8], &h[9],
			&h[10], &h[11], &h[12], &h[13], &h[14], &h[15], &h[16], &h[17], &h[18], &h[19]); err != nil {
			return err
		}
		hists[k] = h
	}
	histRows.Close()

	tx := db.Begin()
	defer tx.Rollback()
	for _, a := range aggs {
		h := hists[histKey{a.Day, a.Route, a.Model, a.Provider, a.Pool, a.Status}]
		err := tx.Exec(`
INSERT INTO request_log_daily
  (day, route, model, provider, pool, status,
   total, success, errors, prompt_tokens, completion_tokens, cost, retries_sum,
   ttftb0, ttftb1, ttftb2, ttftb3, ttftb4, ttftb5, ttftb6, ttftb7, ttftb8, ttftb9,
   totalb0, totalb1, totalb2, totalb3, totalb4, totalb5, totalb6, totalb7, totalb8, totalb9,
   updated_at)
VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?, ?)
ON CONFLICT(day, route, model, provider, pool, status) DO UPDATE SET
  total=excluded.total, success=excluded.success, errors=excluded.errors,
  prompt_tokens=excluded.prompt_tokens, completion_tokens=excluded.completion_tokens,
  cost=excluded.cost, retries_sum=excluded.retries_sum,
  ttftb0=excluded.ttftb0, ttftb1=excluded.ttftb1, ttftb2=excluded.ttftb2, ttftb3=excluded.ttftb3,
  ttftb4=excluded.ttftb4, ttftb5=excluded.ttftb5, ttftb6=excluded.ttftb6, ttftb7=excluded.ttftb7,
  ttftb8=excluded.ttftb8, ttftb9=excluded.ttftb9,
  totalb0=excluded.totalb0, totalb1=excluded.totalb1, totalb2=excluded.totalb2, totalb3=excluded.totalb3,
  totalb4=excluded.totalb4, totalb5=excluded.totalb5, totalb6=excluded.totalb6, totalb7=excluded.totalb7,
  totalb8=excluded.totalb8, totalb9=excluded.totalb9,
  updated_at=excluded.updated_at`,
			a.Day, a.Route, a.Model, a.Provider, a.Pool, a.Status,
			a.Total, a.Success, a.Errors, a.PTok, a.CTok, a.Cost, a.RetriesSum,
			h[0], h[1], h[2], h[3], h[4], h[5], h[6], h[7], h[8], h[9],
			h[10], h[11], h[12], h[13], h[14], h[15], h[16], h[17], h[18], h[19],
			now,
		).Error
		if err != nil {
			return err
		}
	}
	return tx.Commit().Error
}

// P95FromBuckets 从 10 桶直方反查 p95（毫秒）。
func P95FromBuckets(counts [10]int64, bounds [9]int64) int64 {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := (total*95 + 99) / 100
	var acc int64
	for i := 9; i >= 0; i-- {
		acc += counts[i]
		if acc >= target {
			if i == 9 {
				return bounds[8] * 2
			}
			return bounds[i]
		}
	}
	return 0
}