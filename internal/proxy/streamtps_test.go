package proxy

import (
	"testing"
	"time"
)

func TestStreamTPS(t *testing.T) {
	tests := []struct {
		name     string
		isStream bool
		status   string
		u        usageInfo
		ttft     time.Duration
		total    time.Duration
		want     float64
	}{
		{"stream ok", true, "success", usageInfo{completion: 30}, 500 * time.Millisecond, 1500 * time.Millisecond, 30.0},
		{"non stream zero", false, "success", usageInfo{completion: 30}, 1500 * time.Millisecond, 1500 * time.Millisecond, 0},
		{"error zero", true, "error", usageInfo{completion: 30}, 500 * time.Millisecond, 1500 * time.Millisecond, 0},
		{"estimated zero", true, "success", usageInfo{completion: 30, estimated: true}, 500 * time.Millisecond, 1500 * time.Millisecond, 0},
		{"no completion zero", true, "success", usageInfo{}, 500 * time.Millisecond, 1500 * time.Millisecond, 0},
		// 生成窗口 400ms < 500ms：极端小样本速度失真（>100 tok/s 的噪声），不记录
		{"short window zero", true, "success", usageInfo{completion: 30}, 500 * time.Millisecond, 900 * time.Millisecond, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := streamTPS(tt.isStream, tt.status, tt.u, tt.ttft, tt.total)
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("streamTPS() = %v, want %v", got, tt.want)
			}
		})
	}
}
