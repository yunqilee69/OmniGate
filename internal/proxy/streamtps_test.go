package proxy

import (
	"bytes"
	"encoding/json"
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

func TestProviderTimeoutZeroDefaults(t *testing.T) {
	if got := providerTimeout(0); got != defaultProviderTimeout {
		t.Fatalf("TimeoutMs=0 must default to 120s, got %v", got)
	}
	if got := providerTimeout(-1); got != defaultProviderTimeout {
		t.Fatalf("TimeoutMs<0 must default to 120s, got %v", got)
	}
	if got := providerTimeout(80); got != 80*time.Millisecond {
		t.Fatalf("TimeoutMs=80 want 80ms, got %v", got)
	}
}

func TestRewriteJSONModel(t *testing.T) {
	in := []byte(`{"model":"anthropic/claude-sonnet-4","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	out := rewriteJSONModel(in, "claude-sonnet-4")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "claude-sonnet-4" {
		t.Fatalf("model = %v", got["model"])
	}
	if got["max_tokens"].(float64) != 16 {
		t.Fatalf("max_tokens dropped: %v", got["max_tokens"])
	}

	same := rewriteJSONModel(in, "anthropic/claude-sonnet-4")
	if !bytes.Equal(same, in) {
		t.Fatalf("identical model should keep original bytes")
	}
	bad := []byte("not-json")
	if got := rewriteJSONModel(bad, "x"); !bytes.Equal(got, bad) {
		t.Fatalf("invalid json should pass through")
	}
}
