package store

import "testing"

// TestP95FromBuckets 直方桶 p95 反查：自低桶累加计数定位 p95 所在桶，返回桶上界。
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
			want:   50,
		},
		{
			name:   "p95 lands in middle bucket",
			counts: [10]int64{0: 5, 5: 95},
			bounds: TTFTBucketBounds,
			want:   2000,
		},
		{
			// 回归：旧实现自高桶向下累加，把「自顶 5%」当成 p95，
			// 双峰分布会塌回 0 号桶（返回 50）。
			name:   "bimodal mass must not collapse to lowest bucket",
			counts: [10]int64{0: 50, 8: 50},
			bounds: TTFTBucketBounds,
			want:   30000,
		},
		{
			// 线上实测分布：360 次请求，桶 0 装 49 条（错误行），桶 5~9 装成功请求。
			name:   "field distribution with error rows in bucket zero",
			counts: [10]int64{0: 49, 5: 52, 6: 170, 7: 44, 8: 43, 9: 2},
			bounds: TTFTBucketBounds,
			want:   30000,
		},
		{
			name:   "p95 exactly at highest closed bucket",
			counts: [10]int64{8: 94, 9: 6},
			bounds: TTFTBucketBounds,
			want:   60000,
		},
		{
			name:   "p95 inside open top bucket",
			counts: [10]int64{9: 100},
			bounds: TTFTBucketBounds,
			want:   60000,
		},
		{
			name:   "single sample uses its own bucket",
			counts: [10]int64{2: 1},
			bounds: TTFTBucketBounds,
			want:   200,
		},
		{
			name:   "total duration bounds",
			counts: [10]int64{6: 10, 8: 90},
			bounds: TotalBucketBounds,
			want:   300000,
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
