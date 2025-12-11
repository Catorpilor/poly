package telegram

import (
	"sync"
	"time"
)

// RateLimiter implements rate limiting for users
type RateLimiter struct {
	mu       sync.Mutex
	users    map[int64]*userRateLimit
	maxReqs  int
	window   time.Duration
}

// userRateLimit tracks rate limit for a specific user
type userRateLimit struct {
	requests []time.Time
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(maxRequests int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		users:   make(map[int64]*userRateLimit),
		maxReqs: maxRequests,
		window:  window,
	}
}

// Allow checks if a user is allowed to make a request
func (r *RateLimiter) Allow(userID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// Get or create user rate limit
	userLimit, exists := r.users[userID]
	if !exists {
		userLimit = &userRateLimit{
			requests: []time.Time{},
		}
		r.users[userID] = userLimit
	}

	// Remove old requests outside the window
	validRequests := []time.Time{}
	cutoff := now.Add(-r.window)
	for _, reqTime := range userLimit.requests {
		if reqTime.After(cutoff) {
			validRequests = append(validRequests, reqTime)
		}
	}

	// Check if user exceeded rate limit
	if len(validRequests) >= r.maxReqs {
		userLimit.requests = validRequests
		return false
	}

	// Add current request
	validRequests = append(validRequests, now)
	userLimit.requests = validRequests

	return true
}

// Reset resets the rate limit for a user
func (r *RateLimiter) Reset(userID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.users, userID)
}

// Cleanup removes old entries to prevent memory leak
func (r *RateLimiter) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-r.window * 2) // Keep entries for 2x the window

	for userID, userLimit := range r.users {
		if len(userLimit.requests) == 0 {
			delete(r.users, userID)
			continue
		}

		// Check if all requests are old
		allOld := true
		for _, reqTime := range userLimit.requests {
			if reqTime.After(cutoff) {
				allOld = false
				break
			}
		}

		if allOld {
			delete(r.users, userID)
		}
	}
}

// StartCleanupRoutine starts a routine to periodically clean up old entries
func (r *RateLimiter) StartCleanupRoutine() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			r.Cleanup()
		}
	}()
}