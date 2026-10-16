package mcp

import (
	"sync"
	"time"
)

// nonceCache remembers (pub, nonce) pairs until exp. The gateway rejects a
// replay inside 2× the freshness window.
type nonceCache struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	ttl    time.Duration
	lastGC time.Time
}

func newNonceCache(ttl time.Duration) *nonceCache {
	return &nonceCache{seen: map[string]time.Time{}, ttl: ttl}
}

// Add reports whether key was already present and still fresh.
func (c *nonceCache) Add(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp, ok := c.seen[key]; ok && now.Before(exp) {
		return true
	}
	c.seen[key] = now.Add(c.ttl)
	if c.lastGC.IsZero() || now.Sub(c.lastGC) > c.ttl {
		for k, exp := range c.seen {
			if !now.Before(exp) {
				delete(c.seen, k)
			}
		}
		c.lastGC = now
	}
	return false
}
