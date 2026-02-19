package server

import (
	"sync"
	"time"
)

// RateLimiter implements a token bucket rate limiter
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	rate     float64 // tokens per second
	capacity int     // max tokens
}

type tokenBucket struct {
	tokens     float64
	lastUpdate time.Time
}

// NewRateLimiter creates a new rate limiter with the given rate (requests per second)
// and burst capacity
func NewRateLimiter(rate float64, capacity int) *RateLimiter {
	return &RateLimiter{
		buckets:  make(map[string]*tokenBucket),
		rate:     rate,
		capacity: capacity,
	}
}

// Allow checks if a request for the given key should be allowed
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, exists := r.buckets[key]
	now := time.Now()

	if !exists {
		// Create new bucket with full capacity
		r.buckets[key] = &tokenBucket{
			tokens:     float64(r.capacity) - 1, // consume one token
			lastUpdate: now,
		}
		return true
	}

	// Calculate tokens to add based on time elapsed
	elapsed := now.Sub(bucket.lastUpdate).Seconds()
	bucket.tokens += elapsed * r.rate
	bucket.lastUpdate = now

	// Cap at capacity
	if bucket.tokens > float64(r.capacity) {
		bucket.tokens = float64(r.capacity)
	}

	// Try to consume a token
	if bucket.tokens >= 1 {
		bucket.tokens--
		return true
	}

	return false
}

// Remaining returns the number of tokens remaining for a key
func (r *RateLimiter) Remaining(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, exists := r.buckets[key]
	if !exists {
		return r.capacity
	}

	// Update tokens based on time elapsed
	elapsed := time.Since(bucket.lastUpdate).Seconds()
	tokens := bucket.tokens + elapsed*r.rate
	if tokens > float64(r.capacity) {
		tokens = float64(r.capacity)
	}

	return int(tokens)
}

// Reset resets the rate limiter for a specific key
func (r *RateLimiter) Reset(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.buckets, key)
}

// Cleanup removes stale buckets that haven't been used recently
func (r *RateLimiter) Cleanup(maxAge time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	for key, bucket := range r.buckets {
		if bucket.lastUpdate.Before(cutoff) {
			delete(r.buckets, key)
		}
	}
}

// StartCleanup starts a background goroutine to periodically clean up stale buckets
func (r *RateLimiter) StartCleanup(interval, maxAge time.Duration, done <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.Cleanup(maxAge)
			}
		}
	}()
}
