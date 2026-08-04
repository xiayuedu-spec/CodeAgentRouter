package ratelimit

import (
	"sync"
	"time"
)

// Limiter is an in-memory per-user sliding window rate limiter.
type Limiter struct {
	mu      sync.Mutex
	windows map[string][]time.Time
	limit   int
	window  time.Duration
}

func New(limit int, window time.Duration) *Limiter {
	return &Limiter{
		limit:   limit,
		window:  window,
		windows: make(map[string][]time.Time),
	}
}

// Allow records the hit when the user is inside the limit. When denied it
// returns how long to wait before the next request is accepted.
func (l *Limiter) Allow(userID string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	hits := l.windows[userID]
	cutoff := now.Add(-l.window)
	kept := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) < l.limit {
		l.windows[userID] = append(kept, now)
		return true, 0
	}
	l.windows[userID] = kept
	retry := l.window - now.Sub(kept[0])
	if retry < 0 {
		retry = 0
	}
	return false, retry
}
