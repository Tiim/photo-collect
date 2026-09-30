// Package ratelimit provides an in-memory token bucket limiter keyed by string.
package ratelimit

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// sweepInterval is how often idle keys are evicted.
	sweepInterval = time.Minute
	// maxKeys bounds memory: when exceeded, arbitrary keys are dropped.
	maxKeys = 100_000
)

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// Limiter allows perMinute events per key, with a burst of perMinute.
// A nil *Limiter allows everything, so a disabled limit needs no special casing.
type Limiter struct {
	limit  rate.Limit
	burst  int
	window time.Duration // time to refill an empty bucket; idle keys older than this are evicted
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]*entry
	lastSweep time.Time
}

// New returns a limiter, or nil (unlimited) when perMinute <= 0.
func New(perMinute int) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	return &Limiter{
		limit:  rate.Limit(float64(perMinute) / 60),
		burst:  perMinute,
		window: time.Minute,
		now:    time.Now,
		keys:   map[string]*entry{},
	}
}

// Allow consumes one token for key. When none is available it returns false
// and how long the caller should wait before retrying (at least one second).
func (l *Limiter) Allow(key string) (retryAfter time.Duration, ok bool) {
	if l == nil {
		return 0, true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	e := l.keys[key]
	if e == nil {
		if len(l.keys) >= maxKeys {
			l.evictSome()
		}
		e = &entry{lim: rate.NewLimiter(l.limit, l.burst)}
		l.keys[key] = e
	}
	e.seen = now
	r := e.lim.ReserveN(now, 1)
	if !r.OK() {
		return time.Second, false
	}
	d := r.DelayFrom(now)
	if d <= 0 {
		return 0, true
	}
	r.CancelAt(now) // do not consume a token for a rejected request
	return max(time.Second, time.Duration(math.Ceil(d.Seconds()))*time.Second), false
}

// Len returns the number of tracked keys.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for k, e := range l.keys {
		if now.Sub(e.seen) > l.window {
			delete(l.keys, k) // bucket has refilled completely: same as a fresh key
		}
	}
}

// evictSome drops about a tenth of the keys (map order is random).
func (l *Limiter) evictSome() {
	n := len(l.keys)/10 + 1
	for k := range l.keys {
		delete(l.keys, k)
		if n--; n == 0 {
			return
		}
	}
}
