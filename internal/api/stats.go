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

func percentile95(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
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

func (s *Server) rollupHasData(dayFrom, dayTo int64) bool {
	var n int64
	if err := s.store.DB.Table("request_log_daily").
		Where("day BETWEEN ? AND ?", dayFrom, dayTo).
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

	if s.rollupHasData(dayFrom, dayTo) {
		s.overviewFromRollup(w, dayFrom, dayTo, rate)
		return
	}
	s.overviewFromRaw(w, from, to, rate)
}

// overviewFromRollup 走预聚合：单次 SUM 扫描返回所有标量 + 直方桶均值/p95。
func (s *Server) overviewFromRollup(w http.ResponseWriter, dayFrom, dayTo int64, rate float64) {
	var agg struct {
		Total, Success, Errors, PTok, CTok int64
		Cost                               float64
	}
	ttftCounts := [10]int64{}
	totalCounts := [10]int64{}

	row := s.store.DB.Raw(`SELECT
		COALESCE(SUM(total),0), COALESCE(SUM(success),0), COALESCE(SUM(errors),0),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cost),0)
		FROM request_log_daily WHERE day BETWEEN ? AND ?`, dayFrom, dayTo).Row()
	if err := row.Scan(&agg.Total, &agg.Success, &agg.Errors, &agg.PTok, &agg.CTok, &agg.Cost); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	bucketRow := s.store.DB.Raw(`SELECT
		COALESCE(SUM(ttftb0),0), COALESCE(SUM(ttftb1),0), COALESCE(SUM(ttftb2),0), COALESCE(SUM(ttftb3),0),
		COALESCE(SUM(ttftb4),0), COALESCE(SUM(ttftb5),0), COALESCE(SUM(ttftb6),0), COALESCE(SUM(ttftb7),0),
		COALESCE(SUM(ttftb8),0), COALESCE(SUM(ttftb9),0),
		COALESCE(SUM(totalb0),0), COALESCE(SUM(totalb1),0), COALESCE(SUM(totalb2),0), COALESCE(SUM(totalb3),0),
		COALESCE(SUM(totalb4),0), COALESCE(SUM(totalb5),0), COALESCE(SUM(totalb6),0), COALESCE(SUM(totalb7),0),
		COALESCE(SUM(totalb8),0), COALESCE(SUM(totalb9),0)
		FROM request_log_daily WHERE day BETWEEN ? AND ?`, dayFrom, dayTo).Row()
	if err := bucketRow.Scan(
		&ttftCounts[0], &ttftCounts[1], &ttftCounts[2], &ttftCounts[3], &ttftCounts[4],
		&ttftCounts[5], &ttftCounts[6], &ttftCounts[7], &ttftCounts[8], &ttftCounts[9],
		&totalCounts[0], &totalCounts[1], &totalCounts[2], &totalCounts[3], &totalCounts[4],
		&totalCounts[5], &totalCounts[6], &totalCounts[7], &totalCounts[8], &totalCounts[9],
	); err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	var fallbackCount int64
	fallbackRow := s.store.DB.Raw(`SELECT COALESCE(COUNT(*),0) FROM request_log 
		WHERE created_at BETWEEN ? AND ? AND is_fallback = 1`,
		dayFrom*86400, (dayTo+1)*86400).Row()
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

	writeJSON(w, http.StatusOK, map[string]any{
		"total": agg.Total, "success": agg.Success, "errors": agg.Errors, "success_rate": successRate,
		"prompt_tokens": agg.PTok, "completion_tokens": agg.CTok,
		"total_tokens":  agg.PTok + agg.CTok,
		"cost":          agg.Cost * rate,
		"avg_ttft_ms":   avgTTFT, "avg_total_ms": avgTotal,
		"p95_ttft_ms":   p95TTFT, "p95_total_ms": p95Total,
		"fallback_count": fallbackCount,
		"fallback_rate":  fallbackRate,
	})
}

// overviewFromRaw rollup 不可用时的回退路径（测试夹具、冷启动首请求）。
func (s *Server) overviewFromRaw(w http.ResponseWriter, from, to int64, rate float64) {
	where := "created_at BETWEEN ? AND ?"
	args := []any{from, to}

	var agg struct {
		Total    int64
		Success  int64
		Errors   int64
		PTokens  int64
		CTokens  int64
		Cost     float64
		AvgTTFT  sql.NullFloat64
		AvgTotal sql.NullFloat64
	}
	row := s.store.DB.Raw(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cost),0),
		AVG(CASE WHEN status='success' THEN ttft_ms END),
		AVG(CASE WHEN status='success' THEN total_ms END)
		FROM request_log WHERE `+where, args...).Row()
	if err := row.Scan(&agg.Total, &agg.Success, &agg.Errors, &agg.PTokens, &agg.CTokens, &agg.Cost,
		&agg.AvgTTFT, &agg.AvgTotal); err != nil {
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
		"prompt_tokens": agg.PTokens, "completion_tokens": agg.CTokens,
		"total_tokens": agg.PTokens + agg.CTokens,
		"cost":         agg.Cost * rate,
		"avg_ttft_ms":  agg.AvgTTFT.Float64, "avg_total_ms": agg.AvgTotal.Float64,
		"p95_ttft_ms":  percentile95(ttfts), "p95_total_ms": percentile95(totals),
		"fallback_count": fallbackCount,
		"fallback_rate":  fallbackRate,
	})
}

var breakdownDims = map[string]string{
	"route": "route", "model": "model", "provider": "provider",
	"status": "status", "key": "CAST(key_id AS TEXT)", "error_code": "error_code",
}

// key / error_code 维度不在 request_log_daily 预聚合表里，必须走原始表。
var rollupUnsupported = map[string]bool{"key": true, "error_code": true}

func (s *Server) getStatsBreakdown(w http.ResponseWriter, r *http.Request) {
	dim := r.URL.Query().Get("dim")
	col, ok := breakdownDims[dim]
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"dim must be one of route|model|provider|status|key|error_code")
		return
	}
	from, to := parseTimeRange(r)
	dayFrom, dayTo := dayRange(from, to)
	rate := s.costRate(r)

	if !rollupUnsupported[dim] && s.rollupHasData(dayFrom, dayTo) {
		s.breakdownFromRollup(w, col, dayFrom, dayTo, rate)
		return
	}
	s.breakdownFromRaw(w, col, from, to, rate)
}

// breakdownFromRollup 走预聚合：GROUP BY 维度键，扫描 O(天×维度组合)。
func (s *Server) breakdownFromRollup(w http.ResponseWriter, col string, dayFrom, dayTo int64, rate float64) {
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
		AvgRetries float64 `json:"avg_retries"`
	}
	q := `SELECT ` + col + ` AS dim,
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
		SUM(retries_sum) AS retries_sum
		FROM request_log_daily WHERE day BETWEEN ? AND ?
		GROUP BY dim ORDER BY total DESC LIMIT 200`
	rows, err := s.store.DB.Raw(q, dayFrom, dayTo).Rows()
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
		var retriesSum int64
		if err := rows.Scan(&dim, &it.Total, &it.Success, &it.Errors, &it.PromptTok, &it.ComplTok,
			&it.Cost,
			&t[0], &t[1], &t[2], &t[3], &t[4], &t[5], &t[6], &t[7], &t[8], &t[9],
			&o[0], &o[1], &o[2], &o[3], &o[4], &o[5], &o[6], &o[7], &o[8], &o[9],
			&retriesSum); err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		it.Dim = dim.String
		it.AvgTTFT = avgFromBuckets(t, store.TTFTBucketBounds)
		it.AvgTotal = avgFromBuckets(o, store.TotalBucketBounds)
		if it.Total > 0 {
			it.AvgRetries = float64(retriesSum) / float64(it.Total)
		}
		it.Cost *= rate
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, items)
}

// breakdownFromRaw rollup 不可用时的回退路径。
func (s *Server) breakdownFromRaw(w http.ResponseWriter, col string, from, to int64, rate float64) {
	rows, err := s.store.DB.Raw(`SELECT `+col+` AS dim, COUNT(*) AS total,
		SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) AS success,
		SUM(CASE WHEN status='error' THEN 1 ELSE 0 END) AS errors,
		COALESCE(SUM(prompt_tokens),0) AS p_tokens,
		COALESCE(SUM(completion_tokens),0) AS c_tokens,
		COALESCE(SUM(cost),0) AS cost,
		AVG(CASE WHEN status='success' THEN ttft_ms END) AS avg_ttft,
		AVG(CASE WHEN status='success' THEN total_ms END) AS avg_total,
		COALESCE(AVG(retries),0) AS avg_retries
		FROM request_log WHERE created_at BETWEEN ? AND ?
		GROUP BY dim ORDER BY total DESC LIMIT 200`, from, to).Rows()
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
		AvgRetries float64 `json:"avg_retries"`
		KeyMasked  string  `json:"key_masked,omitempty"`
		KeyName    string  `json:"key_name,omitempty"`
	}
	items := []item{}
	for rows.Next() {
		var it item
		var ttft, total sql.NullFloat64
		var dim sql.NullString
		if err := rows.Scan(&dim, &it.Total, &it.Success, &it.Errors, &it.PromptTok, &it.ComplTok,
			&it.Cost, &ttft, &total, &it.AvgRetries); err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		it.Dim = dim.String
		it.AvgTTFT, it.AvgTotal = ttft.Float64, total.Float64
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
	conds := []string{"created_at BETWEEN ? AND ?"}
	args := []any{bucket, bucket, from, to}
	q := r.URL.Query()
	if v := q.Get("route"); v != "" {
		conds = append(conds, "route = ?")
		args = append(args, v)
	}
	if v := q.Get("model"); v != "" {
		conds = append(conds, "model = ?")
		args = append(args, v)
	}
	if v := q.Get("status"); v != "" {
		conds = append(conds, "status = ?")
		args = append(args, v)
	}
	where := strings.Join(conds, " AND ")
	rows, err := s.store.DB.Raw(`SELECT (created_at / ?) * ? AS bucket,
		COUNT(*) AS total,
		SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) AS success,
		COALESCE(SUM(cost),0) AS cost,
		COALESCE(SUM(prompt_tokens+completion_tokens),0) AS total_tokens,
		AVG(CASE WHEN status='success' THEN ttft_ms END) AS avg_ttft,
		AVG(CASE WHEN status='success' THEN total_ms END) AS avg_total
		FROM request_log WHERE `+where+` GROUP BY bucket ORDER BY bucket`, args...).Rows()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	defer rows.Close()
	type point struct {
		Bucket      int64   `json:"bucket"`
		Total       int64   `json:"total"`
		Success     int64   `json:"success"`
		Cost        float64 `json:"cost"`
		TotalTokens int64   `json:"total_tokens"`
		AvgTTFT     float64 `json:"avg_ttft_ms"`
		AvgTotal    float64 `json:"avg_total_ms"`
	}
	points := []point{}
	for rows.Next() {
		var p point
		var ttft, total sql.NullFloat64
		if err := rows.Scan(&p.Bucket, &p.Total, &p.Success, &p.Cost, &p.TotalTokens, &ttft, &total); err != nil {
			writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		p.AvgTTFT, p.AvgTotal = ttft.Float64, total.Float64
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
	if v := q.Get("status"); v != "" {
		conds = append(conds, "r.status = ?")
		args = append(args, v)
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
	}
	var rows []logRow
	if err := s.store.DB.Table("request_log r").
		Select("r.*, COALESCE(k.key_value, '') AS raw_key, COALESCE(k.name, '') AS key_name").
		Joins("LEFT JOIN api_key k ON r.key_id = k.id").
		Where(where, args...).Order("r.id DESC").Limit(pageSize).Offset(offset).Find(&rows).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	type logItem struct {
		store.RequestLog
		KeyValueMasked string `json:"key_value_masked"`
		KeyName        string `json:"key_name"`
	}
	items := make([]logItem, 0, len(rows))
	for _, r := range rows {
		item := logItem{RequestLog: r.RequestLog, KeyValueMasked: maskKey(r.RawKey), KeyName: r.KeyName}
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
	var attempts []store.RequestAttempt
	if err := s.store.DB.Where("request_id = ?", requestID).
		Order("attempt asc, id asc").Find(&attempts).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if attempts == nil {
		attempts = []store.RequestAttempt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"log":              log,
		"key_value_masked": maskedKey,
		"key_name":         keyName,
		"attempts":         attempts,
	})
}

func (s *Server) getLogAttempts(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "request_id")
	var rows []store.RequestAttempt
	if err := s.store.DB.Where("request_id = ?", requestID).
		Order("attempt asc, id asc").Find(&rows).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if rows == nil {
		rows = []store.RequestAttempt{}
	}
	writeJSON(w, http.StatusOK, rows)
}
