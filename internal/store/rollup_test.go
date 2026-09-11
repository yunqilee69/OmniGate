package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestP95FromBuckets 直方桶 p95 反查：自低桶累加计数定位 p95 所在桶，再在桶内线性插值。
func TestP95FromBuckets(t *testing.T) {
	tests := []struct {
		name   string
		counts [10]int64
		bounds [9]int64
		want   int64
	}{
		{
			name:   "empty histogram",
			counts: [10]int64{},
			bounds: TTFTBucketBounds,
			want:   0,
		},
		{
			name:   "all under first bound",
			counts: [10]int64{0: 10},
			bounds: TTFTBucketBounds,
			want:   48,
		},
		{
			name:   "p95 lands in middle bucket",
			counts: [10]int64{0: 5, 5: 95},
			bounds: TTFTBucketBounds,
			want:   1947,
		},
		{
			// 回归：旧实现自高桶向下累加，把「自顶 5%」当成 p95，
			// 双峰分布会塌回 0 号桶（返回 50）。
			name:   "bimodal mass must not collapse to lowest bucket",
			counts: [10]int64{0: 50, 8: 50},
			bounds: TTFTBucketBounds,
			want:   28000,
		},
		{
			// 线上实测分布：360 次请求，桶 0 装 49 条（错误行），桶 5~9 装成功请求。
			// 旧实现直接返回桶 8 上界 30000，插值后落在桶内 62.8% 处。
			name:   "field distribution with error rows in bucket zero",
			counts: [10]int64{0: 49, 5: 52, 6: 170, 7: 44, 8: 43, 9: 2},
			bounds: TTFTBucketBounds,
			want:   22558,
		},
		{
			name:   "p95 just spills past last closed bound",
			counts: [10]int64{8: 94, 9: 6},
			bounds: TTFTBucketBounds,
			want:   35000,
		},
		{
			name:   "p95 inside open top bucket",
			counts: [10]int64{9: 100},
			bounds: TTFTBucketBounds,
			want:   58500,
		},
		{
			name:   "single sample uses its own bucket",
			counts: [10]int64{2: 1},
			bounds: TTFTBucketBounds,
			want:   195,
		},
		{
			name:   "total duration bounds",
			counts: [10]int64{6: 10, 8: 90},
			bounds: TotalBucketBounds,
			want:   290000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := P95FromBuckets(tt.counts, tt.bounds); got != tt.want {
				t.Errorf("P95FromBuckets(%v) = %d, want %d", tt.counts, got, tt.want)
			}
		})
	}
}

// TestBackfillKeepsExistingDaily 回归：明细可被 clear-logs / 保留期单独删除而日聚合保留
// （design.md §8.1）。重启回填只能用残缺明细补缺失的日，不得覆盖已存在的日聚合行。
func TestBackfillKeepsExistingDaily(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "rollup.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().Unix()
	day, prevDay := DayKey(now), DayKey(now-86400)
	// 当天日聚合：明细清空前累计的 100 次成功，已被保留
	if err := st.DB.Create(&RequestLogDaily{
		Day: day, Route: "glm", Model: "m1", Provider: "p1", Status: "success",
		Total: 100, Success: 100, TTFTB6: 100,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 明细：清空后只残留 1 条当天请求，另一天整体只有明细、没有日聚合
	for _, l := range []RequestLog{
		{RequestID: "kept", Route: "glm", Model: "m1", Provider: "p1", Status: "success",
			CreatedAt: now, TTFTMs: 3000, TotalMs: 4000},
		{RequestID: "filled", Route: "glm", Model: "m1", Provider: "p1", Status: "success",
			CreatedAt: now - 86400, TTFTMs: 120, TotalMs: 300},
	} {
		if err := st.DB.Create(&l).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Backfill(st.DB); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var kept RequestLogDaily
	if err := st.DB.Where("day = ? AND route = ? AND status = ?", day, "glm", "success").First(&kept).Error; err != nil {
		t.Fatal(err)
	}
	if kept.Total != 100 || kept.TTFTB6 != 100 {
		t.Fatalf("existing daily row was clobbered by partial raw logs: %+v", kept)
	}

	var filled RequestLogDaily
	if err := st.DB.Where("day = ? AND route = ? AND status = ?", prevDay, "glm", "success").First(&filled).Error; err != nil {
		t.Fatalf("day missing from daily table must be backfilled: %v", err)
	}
	if filled.Total != 1 || filled.TTFTB2 != 1 {
		t.Fatalf("backfilled day wrong: %+v", filled)
	}
}
