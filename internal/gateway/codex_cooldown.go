package gateway

import (
	"context"
	"sync"
	"time"
)

// Only fresh, full-size messages use this cache. Kept sockets can still
// carry a small delta even when the complete history is several megabytes.
const coldTurnMinBytes = 1 << 20
const coldTurnCooldown = 5 * time.Minute
const maxColdTurns = 128

var coldTurns = new(coldTurnCache)

type coldRetryKey struct{}

// Keep the HTTP decision across attempts of one request, even when the
// conversation cooldown expires while an HTTP upload is in flight.
func withColdRetry(ctx context.Context) context.Context {
	if ctx.Value(coldRetryKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, coldRetryKey{}, new(sync.Map))
}

type coldTurnCache struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (c *coldTurnCache) blocked(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Before(c.until[key]) {
		return true
	}
	delete(c.until, key)
	return false
}

func (c *coldTurnCache) remember(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until == nil {
		c.until = make(map[string]time.Time)
	}
	if _, exists := c.until[key]; !exists && len(c.until) >= maxColdTurns {
		var oldest string
		for k, expiry := range c.until {
			if oldest == "" || expiry.Before(c.until[oldest]) {
				oldest = k
			}
		}
		delete(c.until, oldest)
	}
	c.until[key] = now.Add(coldTurnCooldown)
}
