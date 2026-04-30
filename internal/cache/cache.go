package cache

import (
	"sync"
	"time"
)

type entry struct {
	value     any
	expiresAt time.Time
}

// TTLCache is a tiny in-memory cache with per-entry TTL.
// Suitable for small static lookup tables (routes, stop summaries, etc.).
type TTLCache struct {
	mu      sync.RWMutex
	entries map[string]entry
	ttl     time.Duration
}

func New(ttl time.Duration) *TTLCache {
	return &TTLCache{
		entries: make(map[string]entry),
		ttl:     ttl,
	}
}

func (c *TTLCache) Get(key string) (any, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil, false
	}
	return e.value, true
}

func (c *TTLCache) Set(key string, value any) {
	c.mu.Lock()
	c.entries[key] = entry{value: value, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}

func (c *TTLCache) SetWithTTL(key string, value any, ttl time.Duration) {
	if ttl <= 0 {
		ttl = c.ttl
	}
	c.mu.Lock()
	c.entries[key] = entry{value: value, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
}

func (c *TTLCache) Invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *TTLCache) Clear() {
	c.mu.Lock()
	c.entries = make(map[string]entry)
	c.mu.Unlock()
}
