package proxy

import (
	"net/url"
	"testing"
)

// TestCaptureURL：内容捕获落库的出站 URL 必须是完整地址（scheme+host+path+query），
// 且查询串里的凭据参数值脱敏——部分提供商把密钥放在 query（?api-key=xxx）。
func TestCaptureURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain path",
			raw:  "https://open.bigmodel.cn/api/paas/v4/chat/completions",
			want: "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		},
		{
			name: "non-sensitive query kept",
			raw:  "https://example.com/v1/chat/completions?api-version=2024-02-01",
			want: "https://example.com/v1/chat/completions?api-version=2024-02-01",
		},
		{
			name: "api key in query masked",
			raw:  "https://generativelanguage.googleapis.com/v1beta/models/gemini:generateContent?key=AIzaSyD-1234567890secret",
			want: "https://generativelanguage.googleapis.com/v1beta/models/gemini:generateContent?key=AIzaSyD-****",
		},
		{
			name: "short secret fully masked",
			raw:  "https://example.com/v1/models?token=short",
			want: "https://example.com/v1/models?token=****",
		},
		{
			name: "sensitive and benign params coexist",
			raw:  "https://example.com/v1/chat/completions?stream=true&api-key=sk-1234567890abcdef",
			want: "https://example.com/v1/chat/completions?stream=true&api-key=sk-12345****",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := captureURL(u); got != tt.want {
				t.Fatalf("captureURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}

	if got := captureURL(nil); got != "" {
		t.Fatalf("nil URL should yield empty string, got %q", got)
	}
}
