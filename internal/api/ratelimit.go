package api

import (
	"math"
	"sync"
	"time"
)

// RateLimit is a per-caller token bucket: PerSecond sustained, Burst at most. Zero values select the defaults.
type RateLimit struct {
	PerSecond float64
	Burst     float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	cfg     RateLimit
	buckets map[string]*bucket
	now     func() time.Time
}

func newLimiter(cfg RateLimit) *limiter {
	if cfg.PerSecond <= 0 {
		cfg.PerSecond = 20
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 100
	}
	return &limiter{cfg: cfg, buckets: map[string]*bucket{}, now: time.Now}
}

// allow spends one token for key, or reports how long until one is available.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 50_000 {
			l.prune(now)
		}
		b = &bucket{tokens: l.cfg.Burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.cfg.Burst, b.tokens+now.Sub(b.last).Seconds()*l.cfg.PerSecond)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.cfg.PerSecond * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

func (l *limiter) prune(now time.Time) {
	full := time.Duration(l.cfg.Burst / l.cfg.PerSecond * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
}
