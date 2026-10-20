package web

import (
	"sync"
	"time"
)

const statsCacheTTL = 30 * time.Second

// Cached values are immutable. Serialize misses to avoid duplicate expensive
// queries on SQLite's single connection, including simultaneous forced refreshes.
type statsCache[T any] struct {
	mu      sync.Mutex
	value   T
	loaded  time.Time
	expires time.Time
}

func (c *statsCache[T]) get(force bool, load func() (T, error)) (T, error) {
	requested := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expires) && (!force || c.loaded.After(requested)) {
		return c.value, nil
	}
	v, err := load()
	if err == nil {
		c.value, c.loaded = v, time.Now()
		c.expires = c.loaded.Add(statsCacheTTL)
	}
	return v, err
}

func (c *statsCache[T]) invalidate() {
	c.mu.Lock()
	c.expires = time.Time{}
	c.mu.Unlock()
}

func (a *App) invalidateStats() {
	a.userStatsCache.invalidate()
	a.overviewStatsCache.invalidate()
}
