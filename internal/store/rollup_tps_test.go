package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTPSBucketIdx(t *testing.T) {
	tests := []struct {
		name string
		tps  float64
		want int
	}{
		{"no sample negative one", 0, -1},
		{"below min bound", 0.5, 0},
		{"bucket 1 lower", 1, 1},
		{"bucket 3 upper exclusive", 19.999, 3},
		{"bucket 4 lower boundary", 20, 4},
		{"bucket 8 lower", 300, 8},
		{"open top bucket", 600, 9},
		{"huge", 5000, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tpsBucketIdx(tt.tps); got != tt.want {
				t.Errorf("tpsBucketIdx(%v) = %d, want %d", tt.tps, got, tt.want)
			}
		})
	}
}

// TestUpsertDailyTPSBucket TPS>0 只进对应桶；TPS=0（非流式/估算）不进任何桶。
func TestUpsertDailyTPSBucket(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "tps.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().Unix()
	UpsertDaily(st.DB, &RequestLog{RequestID: "a", Route: "r", Model: "m", Provider: "p",
		Status: "success", CreatedAt: now, TTFTMs: 500, TotalMs: 1000, CompletionTokens: 24, Tps: 48})
	UpsertDaily(st.DB, &RequestLog{RequestID: "b", Route: "r", Model: "m", Provider: "p",
		Status: "success", CreatedAt: now, TTFTMs: 900, TotalMs: 900})

	var d RequestLogDaily
	if err := st.DB.Where("route = ?", "r").First(&d).Error; err != nil {
		t.Fatal(err)
	}
	// 48 tok/s ∈ [40,80) = 桶 5
	if d.TpsB5 != 1 {
		t.Fatalf("tps bucket wrong: %+v", d)
	}
	var total int64
	for _, c := range [10]int64{d.TpsB0, d.TpsB1, d.TpsB2, d.TpsB3, d.TpsB4, d.TpsB5, d.TpsB6, d.TpsB7, d.TpsB8, d.TpsB9} {
		total += c
	}
	if total != 1 {
		t.Fatalf("exactly one tps sample expected, got %d: %+v", total, d)
	}
}

// TestBackfillTPSBuckets 历史明细回填：与实时口径一致——
// 仅流式成功、completion>0 且生成窗口 >= 500ms 的行按 completion/(gen/1s) 入桶。
func TestBackfillTPSBuckets(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "tps-bf.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().Unix()
	rows := []RequestLog{
		// 流式成功，窗口 1000ms，800 tok → 800 tok/s → 桶 9（>=600 开区间）
		{RequestID: "fast", Route: "r", Model: "m", Provider: "p", Status: "success",
			IsStream: true, CompletionTokens: 800, TTFTMs: 200, TotalMs: 1200, CreatedAt: now},
		// 流式成功，窗口 1000ms，20 tok → 20 tok/s → 桶 4（[20,40)，左闭右开）
		{RequestID: "mid", Route: "r", Model: "m", Provider: "p", Status: "success",
			IsStream: true, CompletionTokens: 20, TTFTMs: 200, TotalMs: 1200, CreatedAt: now},
		// 非流式：ttft==total，窗口为 0，不入桶
		{RequestID: "buffered", Route: "r", Model: "m", Provider: "p", Status: "success",
			CompletionTokens: 50, TTFTMs: 1000, TotalMs: 1000, CreatedAt: now},
		// 窗口 400ms < 500ms：不入桶
		{RequestID: "short", Route: "r", Model: "m", Provider: "p", Status: "success",
			IsStream: true, CompletionTokens: 100, TTFTMs: 100, TotalMs: 500, CreatedAt: now},
		// 失败：不入桶
		{RequestID: "err", Route: "r", Model: "m", Provider: "p", Status: "error",
			IsStream: true, CompletionTokens: 10, TTFTMs: 200, TotalMs: 1200, CreatedAt: now},
	}
	for i := range rows {
		if err := st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Backfill(st.DB); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var d RequestLogDaily
	if err := st.DB.Where("status = ?", "success").First(&d).Error; err != nil {
		t.Fatal(err)
	}
	if d.Total != 4 {
		t.Fatalf("daily total wrong: %+v", d)
	}
	if d.TpsB9 != 1 {
		t.Fatalf("fast row (800 tok/s) should land in open top bucket: %+v", d)
	}
	if d.TpsB4 != 1 {
		t.Fatalf("mid row (20 tok/s) should land in bucket 4 [20,40): %+v", d)
	}
	sum := d.TpsB0 + d.TpsB1 + d.TpsB2 + d.TpsB3 + d.TpsB5 + d.TpsB6 + d.TpsB7 + d.TpsB8
	if sum != 0 {
		t.Fatalf("buffered/short/error rows must not enter buckets: %+v", d)
	}
}
