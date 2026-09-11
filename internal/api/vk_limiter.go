package api

import (
	"sync"
	"time"
)

// vkRateLimiter 虚拟 key 的进程内 RPM 限流器（固定分钟窗口，对齐墙钟）。
//
// 旧实现按请求读写 SQLite 的 vk_rate_limits 表（每请求 1 次 SELECT + 1 次 UPSERT，
// 且清理函数从未被生产代码调用、表会无限增长）；改为进程内计数后请求路径零 DB 开销。
// 窗口状态是瞬时数据：进程重启即重置（旧实现同样只保留一分钟窗口，差异可忽略）。
type vkRateLimiter struct {
	mu     sync.Mutex
	now    func() time.Time
	minute int64           // 当前窗口键（unix 秒对齐到分钟）
	counts map[int64]int64 // vkID → 当前窗口内的请求数
}

func newVKRateLimiter() *vkRateLimiter {
	return &vkRateLimiter{now: time.Now, counts: make(map[int64]int64)}
}

// rolloverLocked 跨入新的一分钟窗口时清空全部计数（窗口对齐墙钟，所有 VK 同步重置）。
func (l *vkRateLimiter) rolloverLocked(now time.Time) {
	m := now.Unix() / 60 * 60
	if m != l.minute {
		l.minute = m
		clear(l.counts)
	}
}

// Allow 检查该 VK 在当前窗口是否仍有配额；limit<=0 表示不限流。
// 只读检查不计数——计数由请求完成后调用 Record 完成（保持"被拒绝/失败的请求不占配额"的原语义）。
func (l *vkRateLimiter) Allow(vkID, limit int64) bool {
	if limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverLocked(l.now())
	return l.counts[vkID] < limit
}

// Record 记录一次该 VK 的请求（handler 返回后调用）。
func (l *vkRateLimiter) Record(vkID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rolloverLocked(l.now())
	l.counts[vkID]++
}
