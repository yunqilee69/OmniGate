package proxy

import (
	"io"
	"sync"
	"time"
)

// idleReader 在两次 Read 之间超过 idle 阈值时触发 onIdle（用于取消底层请求），
// 为流式读取提供空闲超时，而不限制整个流的持续时间。
type idleReader struct {
	r      io.Reader
	idle   time.Duration
	onIdle func()
	timer  *time.Timer
	mu     sync.Mutex
	closed bool
}

func newIdleReader(r io.Reader, idle time.Duration, onIdle func()) *idleReader {
	i := &idleReader{r: r, idle: idle, onIdle: onIdle}
	i.timer = time.AfterFunc(idle, i.fire)
	return i
}

func (i *idleReader) fire() {
	i.mu.Lock()
	cb := i.onIdle
	closed := i.closed
	i.mu.Unlock()
	if closed || cb == nil {
		return
	}
	cb()
}

func (i *idleReader) Read(p []byte) (int, error) {
	i.mu.Lock()
	if !i.closed {
		i.timer.Reset(i.idle)
	}
	i.mu.Unlock()
	n, err := i.r.Read(p)
	if err != nil {
		i.Close()
	}
	return n, err
}

func (i *idleReader) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	i.closed = true
	i.timer.Stop()
}
