package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/cloudomni/omnigate/internal/store"
)

// TestVKRateLimiterAllow 表驱动：未达上限放行、达到上限拒绝、limit<=0 不限流。
func TestVKRateLimiterAllow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name   string
		limit  int64
		record int
		want   bool
	}{
		{"unlimited", 0, 100, true},
		{"under limit", 10, 5, true},
		{"at limit", 10, 10, false},
		{"over limit", 10, 11, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &vkRateLimiter{now: func() time.Time { return base }, counts: make(map[int64]int64)}
			for range tt.record {
				l.Record(1)
			}
			if got := l.Allow(1, tt.limit); got != tt.want {
				t.Errorf("Allow(vk=1, limit=%d) = %v, want %v", tt.limit, got, tt.want)
			}
		})
	}
}

// TestVKRateLimiterWindowRollover 跨分钟窗口自动重置；不同 VK 计数相互隔离。
func TestVKRateLimiterWindowRollover(t *testing.T) {
	base := time.Unix(1_700_000_000, 0) // 对齐分钟整点
	l := &vkRateLimiter{now: func() time.Time { return base }, counts: make(map[int64]int64)}

	for range 3 {
		l.Record(1)
	}
	if l.Allow(1, 3) {
		t.Fatal("3 records at limit 3 must be denied")
	}
	if !l.Allow(2, 3) {
		t.Fatal("vk 2 has no records, must be allowed")
	}

	// 进入下一分钟：全部重置
	l.now = func() time.Time { return base.Add(61 * time.Second) }
	if !l.Allow(1, 3) {
		t.Fatal("new minute window must reset counts")
	}
	l.Record(1)
	if l.Allow(1, 1) {
		t.Fatal("1 record at limit 1 must be denied")
	}
}

// TestVKRateLimitMiddlewareRejects 走完整路由：RPM=2 时前 2 个请求放行，第 3 个 429 并带配额头。
func TestVKRateLimitMiddlewareRejects(t *testing.T) {
	h, st, _ := newTestServerWithStore(t)
	vk := &store.VirtualKey{Name: "limited", Status: "active", RPMLimit: 2}
	if err := st.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if rec := doV1(t, h, "GET", "/v1/models", nil, vk.KeyValue); rec.Code != http.StatusOK {
			t.Fatalf("request %d should pass, got %d", i+1, rec.Code)
		}
	}
	rec := doV1(t, h, "GET", "/v1/models", nil, vk.KeyValue)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request should be 429, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Limit-Requests"); got != "2" {
		t.Fatalf("X-RateLimit-Limit-Requests = %q, want %q", got, "2")
	}
}

// TestVKRateLimitUnlimited RPM=0 的虚拟 key 不受限。
func TestVKRateLimitUnlimited(t *testing.T) {
	h, _, vkToken := newTestServerWithStore(t)
	for i := 0; i < 5; i++ {
		if rec := doV1(t, h, "GET", "/v1/models", nil, vkToken); rec.Code != http.StatusOK {
			t.Fatalf("unlimited request %d should pass, got %d", i+1, rec.Code)
		}
	}
}
