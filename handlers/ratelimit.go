package handlers

import (
	"sync"
	"time"
)

// minIntervalLimiter is a tiny in-memory per-key throttle: it permits an action
// for a key at most once per minInterval. It is used to rate-limit the
// login-test route, which spawns a headless Chromium per call — without a cap an
// authenticated user could DoS the box or drive an SSRF/credential brute loop.
//
// Deliberately minimal: single mutex, lazy map. Fine for a single replica; a
// multi-replica deploy would move this to a shared store (same caveat as the run
// claim lease). Old keys are swept opportunistically so the map cannot grow
// unbounded.
type minIntervalLimiter struct {
	minInterval time.Duration
	mu          sync.Mutex
	last        map[string]time.Time
}

func newMinIntervalLimiter(minInterval time.Duration) *minIntervalLimiter {
	return &minIntervalLimiter{minInterval: minInterval, last: map[string]time.Time{}}
}

// Allow reports whether an action for key is permitted now. When it returns
// true it records the attempt (so the next call within minInterval is denied).
func (l *minIntervalLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.minInterval {
		return false
	}
	l.last[key] = now
	// Opportunistic sweep of stale entries (older than 10× the interval).
	if len(l.last) > 256 {
		cutoff := now.Add(-10 * l.minInterval)
		for k, t := range l.last {
			if t.Before(cutoff) {
				delete(l.last, k)
			}
		}
	}
	return true
}

// tokenBucketLimiter is a per-key token-bucket throttle sized for normal agent
// polling (unlike minIntervalLimiter's one-per-interval spacing, which would
// break a client that reads several runs/artifacts in quick succession). Each
// key gets a bucket that refills at `rate` tokens/second up to `burst`; a read
// costs one token, and a request with an empty bucket is denied (429). It is
// deliberately minimal (single mutex, lazy map, opportunistic sweep) — fine for
// a single replica; a multi-replica deploy would move this to a shared store
// (same caveat as the run-claim lease).
type tokenBucketLimiter struct {
	rate  float64 // tokens added per second
	burst float64 // bucket capacity
	mu    sync.Mutex
	buk   map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newTokenBucketLimiter(rate, burst float64) *tokenBucketLimiter {
	return &tokenBucketLimiter{rate: rate, burst: burst, buk: map[string]*bucket{}}
}

// Allow reports whether a request for key is permitted now, consuming one token
// when it is.
func (l *tokenBucketLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buk[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buk[key] = b
	} else {
		// Refill based on elapsed time, capped at burst.
		b.tokens += now.Sub(b.last).Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	// Opportunistic sweep: drop buckets untouched long enough to be full again.
	if len(l.buk) > 1024 {
		for k, bb := range l.buk {
			if now.Sub(bb.last).Seconds()*l.rate >= l.burst {
				delete(l.buk, k)
			}
		}
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
