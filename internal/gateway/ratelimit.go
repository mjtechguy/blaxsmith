package gateway

import (
	"sync"
	"time"
)

// limiter is a per-key token bucket (§12: per-token and per-org request
// limits against a runaway agent).
//
// ponytail: per replica. With N gateway replicas the effective limit is N×;
// move to a shared counter when the gateway runs more than one replica.
type limiter struct {
	rate, burst float64
	mu          sync.Mutex
	buckets     map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perSecond float64, burst int) *limiter {
	return &limiter{rate: perSecond, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow takes one token, or reports how long until one is available.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	if l == nil || l.rate <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) > 100_000 { // idle buckets are full anyway; start over.
			l.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}
