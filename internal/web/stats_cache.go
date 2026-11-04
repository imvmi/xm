package web

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const statsCacheTTL = 30 * time.Second

// Cached values are immutable. A single flight runs outside the metadata lock;
// invalidation never waits for SQL, and waiters can leave when canceled.
type statsCache[T any] struct {
	mu      sync.Mutex
	value   T
	loaded  time.Time
	expires time.Time
	version uint64
	flight  *statsFlight
}

type statsFlight struct {
	done  chan struct{}
	err   error
	stale bool
}

func (c *statsCache[T]) get(ctx context.Context, force bool, load func() (T, error)) (T, error) {
	requested := time.Now()
	var zero T
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		c.mu.Lock()
		if time.Now().Before(c.expires) && (!force || c.loaded.After(requested)) {
			v := c.value
			c.mu.Unlock()
			return v, nil
		}
		if f := c.flight; f != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-f.done:
				// A canceled loader must not cancel independent waiters.
				if f.err != nil && !f.stale && !errors.Is(f.err, context.Canceled) && !errors.Is(f.err, context.DeadlineExceeded) {
					return zero, f.err
				}
				continue
			}
		}
		f := &statsFlight{done: make(chan struct{})}
		c.flight = f
		version := c.version
		c.mu.Unlock()
		v, err := load()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		c.mu.Lock()
		f.stale = version != c.version
		if err == nil && !f.stale {
			c.value, c.loaded = v, time.Now()
			c.expires = c.loaded.Add(statsCacheTTL)
		}
		f.err = err
		c.flight = nil
		close(f.done)
		c.mu.Unlock()
		if f.stale {
			continue
		}
		return v, err
	}
}

func (c *statsCache[T]) invalidate() {
	c.mu.Lock()
	c.expires = time.Time{}
	c.version++
	c.mu.Unlock()
}

func (a *App) invalidateStats() {
	a.statsMu.Lock()
	for _, c := range a.accountStatsCaches {
		c.invalidate()
	}
	a.statsMu.Unlock()
	a.userStatsCache.invalidate()
	a.overviewStatsCache.invalidate()
}

func (a *App) accountStatsCache(target string) *statsCache[[]ArchiveUserStats] {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if a.accountStatsCaches == nil {
		a.accountStatsCaches = map[string]*statsCache[[]ArchiveUserStats]{}
	}
	c := a.accountStatsCaches[target]
	if c == nil {
		c = &statsCache[[]ArchiveUserStats]{}
		a.accountStatsCaches[target] = c
	}
	return c
}

func (a *App) invalidateAccountStats(targets map[string]bool) {
	a.statsMu.Lock()
	for target := range targets {
		if c := a.accountStatsCaches[target]; c != nil {
			c.invalidate()
		}
	}
	a.statsMu.Unlock()
	a.userStatsCache.invalidate()
	a.overviewStatsCache.invalidate()
}

// A shared quote or media change also affects accounts that archive its
// ancestors. Resolve those owners using the quote and target-post indexes,
// rather than expiring every account's expensive statistics.
func (a *App) invalidateCollectionStats(target string, changed map[string]bool) {
	targets := map[string]bool{target: true}
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		query := `WITH RECURSIVE affected(id) AS (
 SELECT id FROM posts WHERE id IN (` + strings.TrimRight(strings.Repeat("?,", len(chunk)), ",") + `)
 UNION SELECT p.id FROM posts p JOIN affected a ON p.quote_id=a.id
) SELECT DISTINCT t.target FROM affected a JOIN target_posts t ON t.post_id=a.id`
		rows, err := a.store.DB.QueryContext(ctx, query, args...)
		if err != nil {
			a.invalidateStats()
			return
		}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				break
			}
			targets[name] = true
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			a.invalidateStats()
			return
		}
	}
	a.invalidateAccountStats(targets)
}
