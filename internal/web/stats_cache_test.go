package web

import (
	"context"
	"errors"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestStatsCacheCancellationAndGeneration(t *testing.T) {
	var c statsCache[int]
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan int, 1)
	go func() {
		calls := 0
		v, err := c.get(context.Background(), false, func() (int, error) {
			calls++
			if calls == 1 {
				close(started)
				<-release
			}
			return calls, nil
		})
		if err != nil {
			v = -1
		}
		done <- v
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { _, err := c.get(ctx, false, func() (int, error) { return 99, nil }); waiter <- err }()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter blocked")
	}
	invalidated := make(chan struct{})
	go func() { c.invalidate(); close(invalidated) }()
	select {
	case <-invalidated:
	case <-time.After(time.Second):
		t.Fatal("invalidation waited for SQL")
	}
	close(release)
	if v := <-done; v != 2 {
		t.Fatal("old generation returned after invalidation", v)
	}
	if v, err := c.get(context.Background(), false, func() (int, error) { return 99, nil }); err != nil || v != 2 {
		t.Fatal("stale generation populated cache", v, err)
	}
}

func TestStatsCacheCanceledLoaderDoesNotCancelOtherRequests(t *testing.T) {
	var c statsCache[int]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	leader := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, false, func() (int, error) { close(started); <-ctx.Done(); return 0, ctx.Err() })
		leader <- err
	}()
	<-started
	waiter := make(chan int, 1)
	go func() {
		v, err := c.get(context.Background(), false, func() (int, error) { return 42, nil })
		if err != nil {
			v = -1
		}
		waiter <- v
	}()
	cancel()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case v := <-waiter:
		if v != 42 {
			t.Fatal("loader cancellation propagated", v)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter stranded")
	}
}

func TestAccountStatsInvalidationIncludesSharedQuotes(t *testing.T) {
	a := testApp(t)
	quote := archive.Post{ID: "1", Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}
	for i, name := range []string{"alice", "bob", "charlie"} {
		post := archive.Post{ID: []string{"10", "20", "30"}[i], Username: name}
		if name != "charlie" {
			post.Quote = &quote
		}
		if err := a.store.SavePosts(name, []archive.Post{post}); err != nil {
			t.Fatal(err)
		}
	}
	if stats, err := a.loadAccountStats(context.Background(), "alice", false); err != nil || len(stats) != 1 || stats[0].Username != "alice" {
		t.Fatal("targeted statistics", stats, err)
	}
	if len(a.accountStatsCaches) != 1 || a.accountStatsCaches["alice"] == nil {
		t.Fatal("targeted request loaded unrelated accounts")
	}
	if _, err := a.loadAccountStats(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	before := a.accountStatsCache("charlie").loaded
	if _, err := a.store.DB.Exec("UPDATE media SET status='done' WHERE post_id='1'"); err != nil {
		t.Fatal(err)
	}
	a.invalidateCollectionStats("alice", map[string]bool{"1": true})
	stats, err := a.loadAccountStats(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 3 || stats[0].Media != 1 || stats[1].Media != 1 || stats[2].Media != 0 {
		t.Fatal("shared quote owners not refreshed", stats)
	}
	if !a.accountStatsCache("charlie").loaded.Equal(before) {
		t.Fatal("unrelated account reloaded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := archiveUserStatsContext(ctx, a.store.DB, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatal("SQL ignored cancellation", err)
	}
}
