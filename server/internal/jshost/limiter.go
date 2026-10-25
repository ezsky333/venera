package jshost

import (
	"context"
	"sync"
	"time"
)

// HostLimiter 为每个域名维护一个令牌桶，避免对单个站点造成压力。
// 这在"批量订阅下载"场景是必需的：不加限速极易触发 429 或临时封禁。
type HostLimiter struct {
	mu      sync.Mutex
	perHost map[string]*bucket
	rps     float64
	burst   float64
}

type bucket struct {
	tokens   float64
	last     time.Time
}

// NewHostLimiter 创建一个按主机限速的控制器。rps <= 0 表示不限速。
func NewHostLimiter(rps float64) *HostLimiter {
	if rps <= 0 {
		return nil
	}
	return &HostLimiter{
		perHost: map[string]*bucket{},
		rps:     rps,
		burst:   2, // 允许短时 2 个并发请求
	}
}

// Wait 阻塞直到该主机获得一个令牌，或 ctx 结束。
func (l *HostLimiter) Wait(ctx context.Context, host string) error {
	if l == nil {
		return nil
	}
	for {
		l.mu.Lock()
		b, ok := l.perHost[host]
		now := time.Now()
		if !ok {
			b = &bucket{tokens: l.burst, last: now}
			l.perHost[host] = b
		}
		// 补充令牌
		elapsed := now.Sub(b.last).Seconds()
		b.tokens += elapsed * l.rps
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now

		if b.tokens >= 1 {
			b.tokens--
			l.mu.Unlock()
			return nil
		}
		need := (1 - b.tokens) / l.rps
		l.mu.Unlock()

		timer := time.NewTimer(time.Duration(need * float64(time.Second)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}
