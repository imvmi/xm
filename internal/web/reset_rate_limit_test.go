package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"xm/internal/archive"
)

func seedRateResetState(t *testing.T, a *App) {
	t.Helper()
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "paused")
	for k, v := range map[string]string{"cooldown_until": "9999999999", "rate_limit_failures": "4", "request_budget_until": "9999999999", "budget_batch_pending": `["alice"]`, "budget_batch_failed": "1", "cookie_blocked": "login needs attention"} {
		if err := setState(a.store.DB, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=9999999999,requested=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0 WHERE username='paused'"); err != nil {
		t.Fatal(err)
	}
	p := archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Cursor: "resume-here"}}
	if err := a.store.SaveCollectionPage("alice", 0, archive.Page{Posts: []archive.Post{{ID: "100", AuthorID: "42", Username: "alice"}}, Progress: &p}); err != nil {
		t.Fatal(err)
	}
	b, err := a.store.LoadRequestBudget(archive.Cookies{AuthToken: "synthetic", CSRF: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": "0"}); err != nil {
		t.Fatal(err)
	}
}
func TestResetRateLimitClearsLocalStateAndPreservesWork(t *testing.T) {
	a := testApp(t)
	seedRateResetState(t, a)
	cfg := a.Config()
	cfg.PollingPaused = true
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b := login(t, a)
	// Authenticated reset still requires origin and CSRF protection.
	ok(t, b.request("POST", "/api/rate-limit/reset", nil, "http://localhost", false), http.StatusForbidden)
	ok(t, b.request("POST", "/api/rate-limit/reset", nil, "https://elsewhere.invalid", true), http.StatusForbidden)
	unauth := &browserClient{t: t, a: a}
	ok(t, unauth.request("POST", "/api/rate-limit/reset", nil, "http://localhost", false), http.StatusUnauthorized)
	if state(a.store.DB, "cooldown_until") != "9999999999" {
		t.Fatal("unauthorized reset changed state")
	}
	before := time.Now().Unix()
	ok(t, b.request("POST", "/api/rate-limit/reset", nil, "http://localhost", true), 200)
	for _, key := range []string{"cooldown_until", "rate_limit_failures", "request_budget_until", "budget_batch_failed"} {
		if state(a.store.DB, key) != "0" {
			t.Fatal(key)
		}
	}
	if state(a.store.DB, "budget_batch_pending") != `["alice"]` || state(a.store.DB, "cookie_blocked") != "login needs attention" || !a.Config().PollingPaused {
		t.Fatal("unrelated settings changed")
	}
	var count int
	a.store.DB.QueryRow("SELECT COUNT(*) FROM request_budgets").Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
	progress, err := a.store.LoadCollectionProgress("alice", "42")
	if err != nil || progress.Posts.Cursor != "resume-here" {
		t.Fatal(progress, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Posts != 1 || !list[0].Requested || list[0].NextRun < before || list[0].NextRun > time.Now().Unix() {
		t.Fatal(list[0])
	}
	if list[1].Enabled || !list[1].Requested || list[1].NextRun != 9999999999 {
		t.Fatal(list[1])
	}
	budget, err := a.store.LoadRequestBudget(archive.Cookies{AuthToken: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err = budget.Acquire(context.Background(), "UserTweets"); err != nil {
		t.Fatal("budget remained blocked", err)
	}
}
func TestResetRateLimitRejectsActiveBatch(t *testing.T) {
	a := testApp(t)
	seedRateResetState(t, a)
	b := login(t, a)
	a.scheduler.mu.Lock()
	a.scheduler.active = true
	a.scheduler.mu.Unlock()
	ok(t, b.request("POST", "/api/rate-limit/reset", nil, "http://localhost", true), 409)
	if state(a.store.DB, "cooldown_until") != "9999999999" {
		t.Fatal("active reset changed state")
	}
	var count int
	a.store.DB.QueryRow("SELECT COUNT(*) FROM request_budgets").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
}
func TestResetRateLimitRollsBackOnFailure(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite fault injection")
	}
	seedRateResetState(t, a)
	b := login(t, a)
	if _, err := a.store.DB.Exec(`CREATE TRIGGER fail_budget_reset BEFORE DELETE ON request_budgets BEGIN SELECT RAISE(ABORT,'injected reset failure'); END`); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/rate-limit/reset", nil, "http://localhost", true), 500)
	for key, want := range map[string]string{"cooldown_until": "9999999999", "rate_limit_failures": "4", "request_budget_until": "9999999999"} {
		if state(a.store.DB, key) != want {
			t.Fatal("partial reset", key)
		}
	}
}
