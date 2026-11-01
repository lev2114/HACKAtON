// Package ratelimit provides a tiny in-memory fixed-window limiter. It's not
// meant to be bulletproof (no cleanup of stale keys, no distributed state) —
// just enough to make brute-forcing /api/login impractically slow on a LAN
// hackathon demo, which is the actual threat model here.
package ratelimit

import (
	"sync"
	"time"
)

type Limiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	attempts map[string]*bucket
}

type bucket struct {
	count   int
	resetAt time.Time
}

func New(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, attempts: map[string]*bucket{}}
}

// Allow reports whether a request under key is allowed right now. If not,
// retryAfter says how long until the window resets.
func (l *Limiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.attempts[key]
	if !ok || now.After(b.resetAt) {
		l.attempts[key] = &bucket{count: 1, resetAt: now.Add(l.window)}
		return true, 0
	}
	if b.count >= l.max {
		return false, b.resetAt.Sub(now)
	}
	b.count++
	return true, 0
}
