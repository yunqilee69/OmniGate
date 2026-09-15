package api

import (
	"database/sql"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cloudomni/omnigate/internal/store"
)

func parseTimeRange(r *http.Request) (int64, int64) {
	now := time.Now().Unix()
	from, to := now-86400, now
	if v := r.URL.Query().Get("from"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			from = n
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			to = n
		}
	}
	return from, to
}

func dayRange(from, to int64) (int64, int64) {
	dayFrom := store.DayKey(from)
	dayTo := store.DayKey(to)
	if dayTo < dayFrom {
		dayTo = dayFrom
	}
	return dayFrom, dayTo
}

// rollupCoversRange 仅当 from/to 覆盖完整本地自然日时走日聚合。
// 否则会把滚动窗放大成所跨整天：Dashboard「最近 24 小时」会并入昨天整天，
// 「最近 2 小时」若仍落在当天则会并入今天整天（卡片远大于图表）。
func rollupCoversRange(from, to int64) bool {
	if from > to {
		return false
	}
	dayFrom, dayTo := dayRange(from, to)
	start := store.DayStartUnix(dayFrom)
	endExcl := store.NextDayStartUnix(dayTo)
	return from <= start && to >= endExcl-1
}

type statsFilter struct {
	route, model, provider, status string
}

func parseStatsFilter(r *http.Request) statsFilter {
	q := r.URL.Query()
	return statsFilter{
		route:    q.Get("route"),
		model:    q.Get("model"),
		provider: q.Get("provider"),
		status:   q.Get("status"),
	}
}

func (f statsFilter) appendSQL(conds []string, args []any) ([]string, []any) {
	if f.route != "" {
		conds = append(conds, "route = ?")
		args = append(args, f.route)
	}
	if f.model != "" {
		if p, m, ok := strings.Cut(f.model, "/"); ok && p != "" && m != "" {
			conds = append(conds, "provider = ?", "model = ?")
			args = append(args, p, m)
		} else {
			conds = append(conds, "model = ?")
			args = append(args, f.model)
		}
	}
	if f.provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, f.provider)
	}
	if f.status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.status)
	}
	return conds, args
}

func (f statsFilter) where(extra ...string) (string, []any) {
	conds := append([]string{"status <> 'pending'"}, extra...)
	args := []any{}
	conds, args = f.appendSQL(conds, args)
	return strings.Join(conds, " AND "), args
}

func percentile95(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(float64(len(sorted)-1)*0.95)]
}

func percentile95f(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	return sorted[int(float64(len(sorted)-1)*0.95)]
}

func avgFromBuckets(counts [10]int64, bounds [9]int64) float64 {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	mid := func(i int) int64 {
		if i == 0 {
			return bounds[0] / 2
		}
		if i == 9 {
			return bounds[8] * 2
		}
		return (bounds[i-1] + bounds[i]) / 2
	}
	var sum int64
	for i, c := range counts {
		sum += c * mid(i)
	}
	return float64(sum) / float64(total)
}

func cacheHitRate(prompt, cached int64) float64 {
	denom := prompt
	if cached > prompt {
		denom = prompt + cached
	}
	if denom <= 0 {
		return 0
	}
	return float64(cached) / float64(denom)
}

func totalTokens(prompt, completion, cached int64) int64 {
	n := prompt + completion
	if cached > prompt {
		n += cached
	}
	return n
}

func (s *Server) rollupHasData(dayFrom, dayTo int64) bool {
	var n int64
	if err := s.store.DB.Table("request_log_daily").
		Where("day BETWEEN ? AND ? AND status <> 'pending'", dayFrom, dayTo).
		Count(&n).Error; err != nil {
		return false
	}
	return n > 0
}

// costRate 费用输出换算系数：存储层计价基准为 USD，currency=CNY 时按快照汇率放大。
func (s *Server) costRate(r *http.Request) float64 {
	if r.URL.Query().Get("currency") == "CNY" {
		if rate := s.rt.Snapshot().USDCNY; rate > 0 {
			return rate
		}
	}
	return 1
}

func (s *Server) getStatsOverview(w http.ResponseWriter, r *http.Request) {
	from, to := parseTimeRange(r)
	dayFrom, dayTo := dayRange(from, to)
	rate := s.costRate(r)
	f := parseStatsFilter(r)

	if rollupCoversRange(from, to) && s.rollupHasData(dayFrom, dayTo) {
		s.overviewFromRollup(w, dayFrom, dayTo, rate, f)
		return
	}
	s.overviewFromRaw(w, from, to, rate, f)
}

// overviewFromRollup 走预聚合：单次 SUM 扫描返回所有标量 + 直方桶均值/p95。
func (s *Server) overviewFromRollup(w http.ResponseWriter, dayFrom, dayTo int64, rate float64, f statsFilter) {
	var agg struct {
		Total, Success, Errors, PTok, CTok, CachedTok int64
		Cost                                          float64
	}
	ttftCounts := [10]int64{}
	totalCounts := [10]int64{}
	tpsCounts := [10]int64{}

	// 预聚合表按 status 分行：total 需要排除 pending（pending 行不写入，但防御性过滤）。
	scalarWhere, scalarArgs := f.where("day BETWEEN ? AND ?")
	scalarArgs = append([]any{dayFrom, dayTo}, scalarArgs...)
	row := s.store.DB.Raw(`SELECT
		COALESCE(SUM(total),0), COALESCE(SUM(success),0), COALESCE(SUM(errors),0),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
		COALESCE(SUM(cost),0)
		FROM request_log_daily WHERE `+scalarWhere, scalarArgs...).Row()
	if err := row.Scan(&agg.Total, &agg.Success, &agg.Errors, &agg.PTok, &agg.CTok, &agg.CachedTok, &agg.Cost); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	// 延迟直方图只统计成功请求：错误行的 ttft_ms/total_ms 恒为 0，混入会把计数堆进 0 号桶，
	// 拉低均值、扭曲 p95（与 overviewFromRaw 的 status='success' 口径保持一致）。
	bucketWhere, bucketArgs := f.where("day BETWEEN ? AND ?", "status = 'success'")
	bucketArgs = append([]any{dayFrom, dayTo}, bucketArgs...)
	bucketRow := s.store.DB.Raw(`SELECT
		COALESCE(SUM(ttftb0),0), COALESCE(SUM(ttftb1),0), COALESCE(SUM(ttftb2),0), COALESCE(SUM(ttftb3),0),
		COALESCE(SUM(ttftb4),0), COALESCE(SUM(ttftb5),0), COALESCE(SUM(ttftb6),0), COALESCE(SUM(ttftb7),0),
		COALESCE(SUM(ttftb8),0), COALESCE(SUM(ttftb9),0),
		COALESCE(SUM(totalb0),0), COALESCE(SUM(totalb1),0), COALESCE(SUM(totalb2),0), COALESCE(SUM(totalb3),0),
		COALESCE(SUM(totalb4),0), COALESCE(SUM(totalb5),0), COALESCE(SUM(totalb6),0), COALESCE(SUM(totalb7),0),
		COALESCE(SUM(totalb8),0), COALESCE(SUM(totalb9),0),
		COALESCE(SUM(tpsb0),0), COALESCE(SUM(tpsb1),0), COALESCE(SUM(tpsb2),0), COALESCE(SUM(tpsb3),0),
		COALESCE(SUM(tpsb4),0), COALESCE(SUM(tpsb5),0), COALESCE(SUM(tpsb6),0), COALESCE(SUM(tpsb7),0),
		COALESCE(SUM(tpsb8),0), COALESCE(SUM(tpsb9),0)
		FROM request_log_daily WHERE `+bucketWhere, bucketArgs...).Row()
	if err := bucketRow.Scan(
		&ttftCounts[0], &ttftCounts[1], &ttftCounts[2], &ttftCounts[3], &ttftCounts[4],
		&ttftCounts[5], &ttftCounts[6], &ttftCounts[7], &ttftCounts[8], &ttftCounts[9],
		&totalCounts[0], &totalCounts[1], &totalCounts[2], &totalCounts[3], &totalCounts[4],
		&totalCounts[5], &totalCounts[6], &totalCounts[7], &totalCounts[8], &totalCounts[9],
		&tpsCounts[0], &tpsCounts[1], &tpsCounts[2], &tpsCounts[3], &tpsCounts[4],
		&tpsCounts[5], &tpsCounts[6], &tpsCounts[7], &tpsCounts[8], &tpsCounts[9],
	); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	// fallback 计数走明细表：时间窗必须按本地时区换算，不能把 yyyymmdd 当 unix 秒。
	var fallbackCount int64
	fbWhere, fbArgs := f.where("created_at >= ? AND created_at < ?", "is_fallback = 1")
	fbArgs = append([]any{store.DayStartUnix(dayFrom), store.NextDayStartUnix(dayTo)}, fbArgs...)
	fallbackRow := s.store.DB.Raw(`SELECT COALESCE(COUNT(*),0) FROM request_log WHERE `+fbWhere, fbArgs...).Row()
	if err := fallbackRow.Scan(&fallbackCount); err != nil {
		fallbackCount = 0
	}

	successRate := 0.0
	if agg.Total > 0 {
		successRate = float64(agg.Success) / float64(agg.Total)
	}
	fallbackRate := 0.0
	if agg.Total > 0 {
		fallbackRate = float64(fallbackCount) / float64(agg.Total)
	}
	avgTTFT := avgFromBuckets(ttftCounts, store.TTFTBucketBounds)
	avgTotal := avgFromBuckets(totalCounts, store.TotalBucketBounds)
	p95TTFT := store.P95FromBuckets(ttftCounts, store.TTFTBucketBounds)
	p95Total := store.P95FromBuckets(totalCounts, store.TotalBucketBounds)
	avgTPS := avgFromBuckets(tpsCounts, store.TPSBucketBounds)
	p95TPS := store.P95FromBuckets(tpsCounts, store.TPSBucketBounds)

	writeJSON(w, http.StatusOK, map[string]any{
		"total": agg.Total, "success": agg.Success, "errors": agg.Errors, "success_rate": successRate,
		"prompt_tokens": agg.PTok, "completion_tokens": agg.CTok, "cached_tokens": agg.CachedTok,
		"total_tokens":   totalTokens(agg.PTok, agg.CTok, agg.CachedTok),
		"cache_hit_rate": cacheHitRate(agg.PTok, agg.CachedTok),
		"cost":           agg.Cost * rate,
		"avg_ttft_ms":    avgTTFT, "avg_total_ms": avgTotal,
		"p95_ttft_ms": p95TTFT, "p95_total_ms": p95Total,
		"avg_tps": avgTPS, "p95_tps": p95TPS,
		"fallback_count": fallbackCount,
		"fallback_rate":  fallbackRate,
	})
}

// overviewFromRaw rollup 不可用时的回退路径（测试夹具、冷启动首请求）。
func (s *Server) overviewFromRaw(w http.ResponseWriter, from, to int64, rate float64, f statsFilter) {
	where, args := f.where("created_at BETWEEN ? AND ?")
	args = append([]any{from, to}, args...)

	var agg struct {
		Total        int64
		Success      int64
		Errors       int64
		PTokens      int64
		CTokens      int64
		CachedTokens int64
		Cost         float64
		AvgTTFT      sql.NullFloat64
		AvgTotal     sql.NullFloat64
		AvgTPS       sql.NullFloat64
	}
	row := s.store.DB.Raw(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cached_tokens),0),
		COALESCE(SUM(cost),0),
		AVG(CASE WHEN status='success' THEN ttft_ms END),
		AVG(CASE WHEN status='success' THEN total_ms END),
		AVG(CASE WHEN tps > 0 THEN tps END)
		FROM request_log WHERE `+where, args...).Row()
	if err := row.Scan(&agg.Total, &agg.Success, &agg.Errors, &agg.PTokens, &agg.CTokens, &agg.CachedTokens, &agg.Cost,
		&agg.AvgTTFT, &agg.AvgTotal, &agg.AvgTPS); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	var ttfts, totals []int64
	if err := s.store.DB.Raw(`SELECT ttft_ms FROM request_log WHERE status='success' AND `+where, args...).
		Scan(&ttfts).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if err := s.store.DB.Raw(`SELECT total_ms FROM request_log WHERE status='success' AND `+where, args...).
		Scan(&totals).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	// TPS 直接取落库列（计算时已过滤估算/窗口过短的样本，tps>0 即有效）
	var tpsVals []float64
	if err := s.store.DB.Raw(`SELECT tps FROM request_log WHERE tps > 0 AND `+where, args...).
		Scan(&tpsVals).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	var fallbackCount int64
	fallbackRow := s.store.DB.Raw(`SELECT COALESCE(COUNT(*),0) FROM request_log WHERE is_fallback = 1 AND `+where, args...).Row()
	if err := fallbackRow.Scan(&fallbackCount); err != nil {
		fallbackCount = 0
	}

	successRate := 0.0
	if agg.Total > 0 {
		successRate = float64(agg.Success) / float64(agg.Total)
	}
	fallbackRate := 0.0
	if agg.Total > 0 {
		fallbackRate = float64(fallbackCount) / float64(agg.Total)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": agg.Total, "success": agg.Success, "errors": agg.Errors, "success_rate": successRate,
		"prompt_tokens": agg.PTokens, "completion_tokens": agg.CTokens, "cached_tokens": agg.CachedTokens,
		"total_tokens":   totalTokens(agg.PTokens, agg.CTokens, agg.CachedTokens),
		"cache_hit_rate": cacheHitRate(agg.PTokens, agg.CachedTokens),
		"cost":           agg.Cost * rate,
		"avg_ttft_ms":    agg.AvgTTFT.Float64, "avg_total_ms": agg.AvgTotal.Float64,
		"p95_ttft_ms": percentile95(ttfts), "p95_total_ms": percentile95(totals),
		"avg_tps": agg.AvgTPS.Float64, "p95_tps": percentile95f(tpsVals),
		"fallback_count": fallbackCount,
		"fallback_rate":  fallbackRate,
	})
}

var breakdownDims = map[string]string{
	"route": "route", "model": "model", "provider": "provider",
	"status": "status", "key": "CAST(key_id AS TEXT)", "error_code": "error_code",
	"virtual_key": "CAST(vk_id AS TEXT)",
}

// key / error_code 维度不在 request_log_daily 预聚合表里，必须走原始表。
var rollupUnsupported = map[string]bool{"key": true, "error_code": true, "virtual_key": true}

func (s *Server) getStatsBreakdown(w http.ResponseWriter, r *http.Request) {
	dim := r.URL.Query().Get("dim")
	col, ok := breakdownDims[dim]
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"dim must be one of route|model|provider|status|key|error_code|virtual_key")
		return
	}
	from, to := parseTimeRange(r)
	dayFrom, dayTo := dayRange(from, to)
	rate := s.costRate(r)
	f := parseStatsFilter(r)

	if !rollupUnsupported[dim] && rollupCoversRange(from, to) && s.rollupHasData(dayFrom, dayTo) {
		s.breakdownFromRollup(w, col, dayFrom, dayTo, rate, f)
		return
	}
	s.breakdownFromRaw(w, col, from, to, rate, f)
}

// breakdownFromRollup 走预聚合：GROUP BY 维度键，扫描 O(天×维度组合)。
func (s *Server) breakdownFromRollup(w http.ResponseWriter, col string, dayFrom, dayTo int64, rate float64, f statsFilter) {
	type item struct {
		Dim        string  `json:"dim"`
		Total      int64   `json:"total"`
		Success    int64   `json:"success"`
		Errors     int64   `json:"errors"`
		PromptTok  int64   `json:"prompt_tokens"`
		ComplTok   int64   `json:"completion_tokens"`
		Cost       float64 `json:"cost"`
		AvgTTFT    float64 `json:"avg_ttft_ms"`
		AvgTotal   float64 `json:"avg_total_ms"`
		AvgTPS     float64 `json:"avg_tps"`
		AvgRetries float64 `json:"avg_retries"`
	}

	// 直方桶只在成功行累加（见 store.UpsertDaily），跨 status 求和即成功口径。
	where, filterArgs := f.where("day BETWEEN ? AND ?")
	args := append([]any{dayFrom, dayTo}, filterArgs...)

	// model 维度特殊处理：按 provider, model 分组后拼接为 provider/model
	aggCols := `
			SUM(total) AS total,
			SUM(success) AS success,
			SUM(errors) AS errors,
			SUM(prompt_tokens) AS p_tok,
			SUM(completion_tokens) AS c_tok,
			SUM(cost) AS cost,
			SUM(ttftb0) AS tb0, SUM(ttftb1) AS tb1, SUM(ttftb2) AS tb2, SUM(ttftb3) AS tb3, SUM(ttftb4) AS tb4,
			SUM(ttftb5) AS tb5, SUM(ttftb6) AS tb6, SUM(ttftb7) AS tb7, SUM(ttftb8) AS tb8, SUM(ttftb9) AS tb9,
			SUM(totalb0) AS ob0, SUM(totalb1) AS ob1, SUM(totalb2) AS ob2, SUM(totalb3) AS ob3, SUM(totalb4) AS ob4,
			SUM(totalb5) AS ob5, SUM(totalb6) AS ob6, SUM(totalb7) AS ob7, SUM(totalb8) AS ob8, SUM(totalb9) AS ob9,
			SUM(tpsb0) AS pb0, SUM(tpsb1) AS pb1, SUM(tpsb2) AS pb2, SUM(tpsb3) AS pb3, SUM(tpsb4) AS pb4,
			SUM(tpsb5) AS pb5, SUM(tpsb6) AS pb6, SUM(tpsb7) AS pb7, SUM(tpsb8) AS pb8, SUM(tpsb9) AS pb9,
			SUM(retries_sum) AS retries_sum`
	var q string
	if col == "model" {
		q = `SELECT provider || '/' || model AS dim,` + aggCols + `
			FROM request_log_daily WHERE ` + where + `
			GROUP BY provider, model ORDER BY total DESC LIMIT 200`
	} else {
		q = `SELECT ` + col + ` AS dim,` + aggCols + `
			FROM request_log_daily WHERE ` + where + `
			GROUP BY dim ORDER BY total DESC LIMIT 200`
	}

	rows, err := s.store.DB.Raw(q, args...).Rows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	defer rows.Close()
	items := []item{}
	for rows.Next() {
		var it item
		var dim sql.NullString
		var t [10]int64
		var o [10]int64
		var p [10]int64
		var retriesSum int64
		if err := rows.Scan(&dim, &it.Total, &it.Success, &it.Errors, &it.PromptTok, &it.ComplTok,
			&it.Cost,
			&t[0], &t[1], &t[2], &t[3], &t[4], &t[5], &t[6], &t[7], &t[8], &t[9],
			&o[0], &o[1], &o[2], &o[3], &o[4], &o[5], &o[6], &o[7], &o[8], &o[9],
			&p[0], &p[1], &p[2], &p[3], &p[4], &p[5], &p[6], &p[7], &p[8], &p[9],
			&retriesSum); err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		it.Dim = dim.String
		it.AvgTTFT = avgFromBuckets(t, store.TTFTBucketBounds)
		it.AvgTotal = avgFromBuckets(o, store.TotalBucketBounds)
		it.AvgTPS = avgFromBuckets(p, store.TPSBucketBounds)
		if it.Total > 0 {
			it.AvgRetries = float64(retriesSum) / float64(it.Total)
		}
		it.Cost *= rate
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, items)
}

// breakdownFromRaw rollup 不可用时的回退路径。
func (s *Server) breakdownFromRaw(w http.ResponseWriter, col string, from, to int64, rate float64, f statsFilter) {
	where, filterArgs := f.where("created_at BETWEEN ? AND ?")
	args := append([]any{from, to}, filterArgs...)

	aggCols := ` COUNT(*) AS total,
			SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN status='error' THEN 1 ELSE 0 END) AS errors,
			COALESCE(SUM(prompt_tokens),0) AS p_tokens,
			COALESCE(SUM(completion_tokens),0) AS c_tokens,
			COALESCE(SUM(cost),0) AS cost,
			AVG(CASE WHEN status='success' THEN ttft_ms END) AS avg_ttft,
			AVG(CASE WHEN status='success' THEN total_ms END) AS avg_total,
			AVG(CASE WHEN tps > 0 THEN tps END) AS avg_tps,
			COALESCE(AVG(retries),0) AS avg_retries`

	// model 维度特殊处理：按 provider, model 分组后拼接为 provider/model
	var q string
	if col == "model" {
		q = `SELECT provider || '/' || model AS dim,` + aggCols + `
			FROM request_log WHERE ` + where + `
			GROUP BY provider, model ORDER BY total DESC LIMIT 200`
	} else {
		q = `SELECT ` + col + ` AS dim,` + aggCols + `
			FROM request_log WHERE ` + where + `
			GROUP BY dim ORDER BY total DESC LIMIT 200`
	}

	rows, err := s.store.DB.Raw(q, args...).Rows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type item struct {
		Dim        string  `json:"dim"`
		Total      int64   `json:"total"`
		Success    int64   `json:"success"`
		Errors     int64   `json:"errors"`
		PromptTok  int64   `json:"prompt_tokens"`
		ComplTok   int64   `json:"completion_tokens"`
		Cost       float64 `json:"cost"`
		AvgTTFT    float64 `json:"avg_ttft_ms"`
		AvgTotal   float64 `json:"avg_total_ms"`
		AvgTPS     float64 `json:"avg_tps"`
		AvgRetries float64 `json:"avg_retries"`
		KeyMasked  string  `json:"key_masked,omitempty"`
		KeyName    string  `json:"key_name,omitempty"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		var ttft, total sql.NullFloat64
		var tps sql.NullFloat64
		var dim sql.NullString
		if err := rows.Scan(&dim, &it.Total, &it.Success, &it.Errors, &it.PromptTok, &it.ComplTok,
			&it.Cost, &ttft, &total, &tps, &it.AvgRetries); err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		it.Dim = dim.String
		it.AvgTTFT, it.AvgTotal = ttft.Float64, total.Float64
		it.AvgTPS = tps.Float64
		it.Cost *= rate
		items = append(items, it)
	}
	// dim=key 的 dim 是裸 key_id，附名称与脱敏值让客户端不查库即可辨认密钥
	if col == breakdownDims["key"] {
		ids := make([]int64, 0, len(items))
		for i := range items {
			if n, err := strconv.ParseInt(items[i].Dim, 10, 64); err == nil && n > 0 {
				ids = append(ids, n)
			}
		}
		type keyLabel struct{ masked, name string }
		labels := map[int64]keyLabel{}
		if len(ids) > 0 {
			var keys []store.ApiKey
			if err := s.store.DB.Select("id", "key_value", "name").Where("id IN ?", ids).Find(&keys).Error; err == nil {
				for _, k := range keys {
					labels[k.ID] = keyLabel{maskKey(k.KeyValue), k.Name}
				}
			}
		}
		for i := range items {
			if n, err := strconv.ParseInt(items[i].Dim, 10, 64); err == nil {
				items[i].KeyMasked = labels[n].masked
				items[i].KeyName = labels[n].name
			}
		}
	}
	// dim=virtual_key 的 dim 是 vk_id，附名称让客户端识别虚拟密钥
	if col == breakdownDims["virtual_key"] {
		ids := make([]int64, 0, len(items))
		for i := range items {
			if n, err := strconv.ParseInt(items[i].Dim, 10, 64); err == nil && n > 0 {
				ids = append(ids, n)
			}
		}
		vkNames := map[int64]string{}
		if len(ids) > 0 {
			var vks []store.VirtualKey
			if err := s.store.DB.Select("id", "name").Where("id IN ?", ids).Find(&vks).Error; err == nil {
				for _, vk := range vks {
					vkNames[vk.ID] = vk.Name
				}
			}
		}
		for i := range items {
			if n, err := strconv.ParseInt(items[i].Dim, 10, 64); err == nil {
				items[i].KeyName = vkNames[n]
				if items[i].KeyName == "" {
					items[i].KeyName = "Unknown VK"
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getStatsTimeseries(w http.ResponseWriter, r *http.Request) {
	bucket := int64(3600)
	if v := r.URL.Query().Get("bucket"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			bucket = int64(d.Seconds())
		}
	}
	from, to := parseTimeRange(r)
	rate := s.costRate(r)
	f := parseStatsFilter(r)
	where, filterArgs := f.where("created_at BETWEEN ? AND ?")
	args := append([]any{bucket, bucket, from, to}, filterArgs...)
	rows, err := s.store.DB.Raw(`SELECT (created_at / ?) * ? AS bucket,
		COUNT(*) AS total,
		SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) AS success,
		SUM(CASE WHEN status='error' THEN 1 ELSE 0 END) AS errors,
		COALESCE(SUM(prompt_tokens),0) AS prompt_tokens,
		COALESCE(SUM(completion_tokens),0) AS completion_tokens,
		COALESCE(SUM(cost),0) AS cost,
		COALESCE(SUM(prompt_tokens+completion_tokens),0) AS total_tokens,
		SUM(CASE WHEN is_fallback = 1 THEN 1 ELSE 0 END) AS fallback_count,
		AVG(CASE WHEN status='success' THEN ttft_ms END) AS avg_ttft,
		AVG(CASE WHEN status='success' THEN total_ms END) AS avg_total,
		AVG(CASE WHEN tps > 0 THEN tps END) AS avg_tps
		FROM request_log WHERE `+where+` GROUP BY bucket ORDER BY bucket`, args...).Rows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type point struct {
		Bucket           int64   `json:"bucket"`
		Total            int64   `json:"total"`
		Success          int64   `json:"success"`
		Errors           int64   `json:"errors"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
		TotalTokens      int64   `json:"total_tokens"`
		FallbackCount    int64   `json:"fallback_count"`
		AvgTTFT          float64 `json:"avg_ttft_ms"`
		AvgTotal         float64 `json:"avg_total_ms"`
		AvgTPS           float64 `json:"avg_tps"`
	}
	points := []point{}
	for rows.Next() {
		var p point
		var ttft, total sql.NullFloat64
		var tps sql.NullFloat64
		if err := rows.Scan(&p.Bucket, &p.Total, &p.Success, &p.Errors, &p.PromptTokens,
			&p.CompletionTokens, &p.Cost, &p.TotalTokens, &p.FallbackCount, &ttft, &total, &tps); err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		p.AvgTTFT, p.AvgTotal = ttft.Float64, total.Float64
		p.AvgTPS = tps.Float64
		p.Cost *= rate
		points = append(points, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"bucket_s": bucket, "points": points})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	conds := []string{"1=1"}
	args := []any{}
	if v := q.Get("route"); v != "" {
		conds = append(conds, "r.route = ?")
		args = append(args, v)
	}
	if v := q.Get("model"); v != "" {
		conds = append(conds, "r.model = ?")
		args = append(args, v)
	}
	if v := q.Get("provider"); v != "" {
		conds = append(conds, "r.provider = ?")
		args = append(args, v)
	}
	if v := q.Get("status"); v != "" {
		conds = append(conds, "r.status = ?")
		args = append(args, v)
	}
	if v := q.Get("endpoint"); v != "" {
		eps := store.EndpointsForFilter(v)
		if len(eps) == 1 {
			conds = append(conds, "r.endpoint = ?")
			args = append(args, eps[0])
		} else {
			conds = append(conds, "r.endpoint IN ?")
			args = append(args, eps)
		}
	}
	if v := q.Get("vk_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "vk_id must be a positive integer")
			return
		}
		conds = append(conds, "r.vk_id = ?")
		args = append(args, id)
	}
	from, to := parseTimeRange(r)
	conds = append(conds, "r.created_at BETWEEN ? AND ?")
	args = append(args, from, to)
	where := strings.Join(conds, " AND ")

	var total int64
	if err := s.store.DB.Table("request_log r").Where(where, args...).Count(&total).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	page, size := 1, 50
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(q.Get("size")); err == nil && v > 0 && v <= 200 {
		size = v
	}
	pageSize, offset := size, (page-1)*size

	type logRow struct {
		store.RequestLog
		RawKey  string `gorm:"column:raw_key"`
		KeyName string `gorm:"column:key_name"`
		VKName  string `gorm:"column:vk_name"`
	}
	var rows []logRow
	if err := s.store.DB.Table("request_log r").
		Select("r.*, COALESCE(k.key_value, '') AS raw_key, COALESCE(k.name, '') AS key_name, COALESCE(vk.name, '') AS vk_name").
		Joins("LEFT JOIN api_key k ON r.key_id = k.id").
		Joins("LEFT JOIN virtual_key vk ON r.vk_id = vk.id").
		Where(where, args...).Order("r.id DESC").Limit(pageSize).Offset(offset).Find(&rows).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	type logItem struct {
		store.RequestLog
		KeyValueMasked string `json:"key_value_masked"`
		KeyName        string `json:"key_name"`
		VKName         string `json:"vk_name"`
	}
	items := make([]logItem, 0, len(rows))
	for _, r := range rows {
		item := logItem{RequestLog: r.RequestLog, KeyValueMasked: maskKey(r.RawKey), KeyName: r.KeyName, VKName: r.VKName}
		items = append(items, item)
	}
	if items == nil {
		items = []logItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "items": items})
}

func (s *Server) getLogContent(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "request_id")
	var cl store.ContentLog
	err := s.store.DB.Where("request_id = ?", requestID).First(&cl).Error
	if err != nil {
		captureOn := false
		var row store.AppConfig
		if e := s.store.DB.Where("`key` = ?", "capture.enabled").First(&row).Error; e == nil {
			captureOn = row.Value == "true"
		}
		hint := "内容捕获未开启（可在 设置 中启用）"
		if captureOn {
			hint = "该请求无捕获内容（路由不在白名单或捕获前请求）"
		}
		writeErr(w, http.StatusNotFound, "no_content", hint)
		return
	}
	writeJSON(w, http.StatusOK, cl)
}

func (s *Server) getLogByID(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "request_id")
	var log store.RequestLog
	if err := s.store.DB.Where("request_id = ?", requestID).First(&log).Error; err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "log not found")
		return
	}
	maskedKey := ""
	keyName := ""
	if log.KeyID > 0 {
		var k store.ApiKey
		if err := s.store.DB.First(&k, log.KeyID).Error; err == nil {
			maskedKey = maskKey(k.KeyValue)
			keyName = k.Name
		}
	}
	vkName := ""
	if log.VKID > 0 {
		var vk store.VirtualKey
		if err := s.store.DB.First(&vk, log.VKID).Error; err == nil {
			vkName = vk.Name
		}
	}
	attempts, err := s.loadAttempts(requestID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"log":              log,
		"key_value_masked": maskedKey,
		"key_name":         keyName,
		"vk_name":          vkName,
		"attempts":         attempts,
	})
}

func (s *Server) getLogAttempts(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "request_id")
	rows, err := s.loadAttempts(requestID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// attemptRow 尝试明细行 + 经 LEFT JOIN 带出的密钥名称与原始密钥值（密钥删除后仍返回该尝试行）。
type attemptRow struct {
	store.RequestAttempt
	RawKey  string `gorm:"column:raw_key"`
	KeyName string `gorm:"column:key_name"`
}

// attemptItem 尝试明细的 API 输出：内嵌落库字段，附密钥名称与脱敏值，与日志列表的密钥展示口径一致
// （逐跳可能命中不同密钥，必须按 key_id 各自取名而非用请求终态的密钥）。
type attemptItem struct {
	store.RequestAttempt
	KeyValueMasked string `json:"key_value_masked"`
	KeyName        string `json:"key_name"`
}

// loadAttempts 读取某请求的全部尝试明细，并按 key_id 补齐密钥名称/脱敏值。
func (s *Server) loadAttempts(requestID string) ([]attemptItem, error) {
	var rows []attemptRow
	if err := s.store.DB.Table("request_attempt a").
		Select("a.*, COALESCE(k.key_value, '') AS raw_key, COALESCE(k.name, '') AS key_name").
		Joins("LEFT JOIN api_key k ON a.key_id = k.id").
		Where("a.request_id = ?", requestID).
		Order("a.attempt asc, a.id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]attemptItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, attemptItem{
			RequestAttempt: r.RequestAttempt,
			KeyValueMasked: maskKey(r.RawKey),
			KeyName:        r.KeyName,
		})
	}
	return items, nil
}

// VKStatsResponse 虚拟密钥统计响应
type VKStatsResponse struct {
	VKID         int64   `json:"vk_id"`
	VKName       string  `json:"vk_name"`
	Requests     int     `json:"requests"`
	SuccessCount int     `json:"success_count"`
	ErrorCount   int     `json:"error_count"`
	TotalCost    float64 `json:"total_cost"`
}

// GetVKStats 获取虚拟密钥统计：默认最近 24h，可用 from/to/currency 对齐总览。
func (s *Server) GetVKStats(w http.ResponseWriter, r *http.Request) {
	from, to := parseTimeRange(r)
	rate := s.costRate(r)

	type row struct {
		VKID         int64   `gorm:"column:vk_id"`
		Requests     int64   `gorm:"column:requests"`
		SuccessCount int64   `gorm:"column:success_count"`
		ErrorCount   int64   `gorm:"column:error_count"`
		TotalCost    float64 `gorm:"column:total_cost"`
	}
	var rows []row
	if err := s.store.DB.Raw(`
SELECT vk_id AS vk_id,
       COUNT(*) AS requests,
       SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) AS success_count,
       SUM(CASE WHEN status='error' THEN 1 ELSE 0 END) AS error_count,
       COALESCE(SUM(cost),0) AS total_cost
FROM request_log
WHERE created_at BETWEEN ? AND ? AND vk_id > 0 AND status <> 'pending'
GROUP BY vk_id`, from, to).Scan(&rows).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "database_error", "failed to query logs")
		return
	}

	var vks []store.VirtualKey
	if err := s.store.DB.Find(&vks).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "database_error", "failed to query virtual keys")
		return
	}
	vkNameMap := make(map[int64]string, len(vks))
	for _, vk := range vks {
		vkNameMap[vk.ID] = vk.Name
	}

	result := make([]VKStatsResponse, 0, len(rows))
	for _, row := range rows {
		name := vkNameMap[row.VKID]
		if name == "" {
			name = "Unknown"
		}
		result = append(result, VKStatsResponse{
			VKID:         row.VKID,
			VKName:       name,
			Requests:     int(row.Requests),
			SuccessCount: int(row.SuccessCount),
			ErrorCount:   int(row.ErrorCount),
			TotalCost:    row.TotalCost * rate,
		})
	}
	writeJSON(w, http.StatusOK, result)
}
