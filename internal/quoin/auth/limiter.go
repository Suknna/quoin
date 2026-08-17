package auth

import (
	"sync"
	"time"
)

const (
	failureWindow = 15 * time.Minute
	cooldown      = 15 * time.Minute
	failureLimit  = 5
	maxIdentities = 4096
)

type loginState struct {
	failures      []time.Time
	cooldownUntil time.Time
	lastSeen      time.Time
}

type loginLimiter struct {
	mu     sync.Mutex
	states map[string]*loginState
	now    func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{states: make(map[string]*loginState), now: time.Now}
}

func (limiter *loginLimiter) allow(username string) (bool, time.Duration) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.now()
	state := limiter.states[username]
	if state == nil {
		return true, 0
	}
	state.lastSeen = now
	if state.cooldownUntil.After(now) {
		return false, state.cooldownUntil.Sub(now)
	}
	state.failures = retainRecent(state.failures, now)
	return true, 0
}

func (limiter *loginLimiter) failed(username string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.now()
	state := limiter.states[username]
	if state == nil {
		if len(limiter.states) >= maxIdentities {
			limiter.evictOldest()
		}
		state = &loginState{}
		limiter.states[username] = state
	}
	state.lastSeen = now
	state.failures = append(retainRecent(state.failures, now), now)
	if len(state.failures) >= failureLimit {
		state.cooldownUntil = now.Add(cooldown)
	}
}

func (limiter *loginLimiter) succeeded(username string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	delete(limiter.states, username)
}

func (limiter *loginLimiter) evictOldest() {
	var oldestKey string
	var oldest time.Time
	for key, state := range limiter.states {
		if oldestKey == "" || state.lastSeen.Before(oldest) {
			oldestKey, oldest = key, state.lastSeen
		}
	}
	delete(limiter.states, oldestKey)
}

func retainRecent(values []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-failureWindow)
	first := 0
	for first < len(values) && values[first].Before(cutoff) {
		first++
	}
	return values[first:]
}
