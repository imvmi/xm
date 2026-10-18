package web

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestRateLimitDeadline(t *testing.T) {
	const now int64 = 1792274400
	for _, tc := range []struct {
		name               string
		interval, previous int
		retryAt            int64
		wantDelay          int64
		wantFailures       int
	}{
		{"minimum", 60, 0, 0, 900, 1},
		{"respect_hourly_polling", 3600, 0, 0, 3600, 1},
		{"double", 3600, 1, 0, 7200, 2},
		{"double_again", 3600, 2, 0, 14400, 3},
		{"ceiling", 3600, 3, 0, 21600, 4},
		{"large_counter", 3600, int(^uint(0) >> 1), 0, 21600, 16},
		{"long_polling_interval", 86400, 8, 0, 86400, 9},
		{"upstream_later_than_ceiling", 3600, 6, now + 86400, 86405, 7},
		{"upstream_shorter_than_policy", 3600, 1, now + 60, 7200, 2},
		{"negative_counter", 900, -2, 0, 900, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := &archive.Failure{Kind: "rate"}
			if tc.retryAt > 0 {
				failure.RetryAt = time.Unix(tc.retryAt, 0)
			}
			until, failures := rateLimitDeadline(now, tc.interval, tc.previous, fmt.Errorf("wrapped: %w", failure))
			if until != now+tc.wantDelay || failures != tc.wantFailures {
				t.Fatalf("deadline=%d failures=%d, want %d/%d", until, failures, now+tc.wantDelay, tc.wantFailures)
			}
		})
	}
}

func TestSchedulerRepeatedRateLimitsSurviveRestartAndPartialSuccess(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	seedMonitor(t, a, "carol")
	cfg := a.Config()
	cfg.DefaultInterval = 3600
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET catchup=1,catchup_post_anchor='100',catchup_reply_anchor='90',catchup_scroll_budget=20 WHERE username='bob'"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	bobFailures := 0
	lastTarget := ""
	failBob := true
	collector := func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, identity archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		calls++
		lastTarget = target
		if target == "bob" && failBob {
			bobFailures++
			if !identity.Catchup || identity.CatchupPostAnchor != "100" || identity.CatchupReplyAnchor != "90" || identity.CatchupScrollBudget != 20 {
				t.Fatalf("rate retry changed the checkpoint/budget: %+v", identity)
			}
			return archive.Collection{}, &archive.Failure{Kind: "rate", Message: "synthetic rate limit"}
		}
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	for attempt, delay := range []int64{3600, 7200, 14400, 21600, 21600} {
		a.scheduler.collect = collector
		before := time.Now().Unix()
		if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
			t.Fatal(worked, err)
		}
		until, err := strconv.ParseInt(state(a.store.DB, "cooldown_until"), 10, 64)
		if err != nil || until < before+delay || until > time.Now().Unix()+delay {
			t.Fatalf("attempt=%d deadline=%d delay=%d err=%v", attempt, until, delay, err)
		}
		if state(a.store.DB, "rate_limit_failures") != strconv.Itoa(attempt+1) || bobFailures != attempt+1 || lastTarget != "bob" {
			t.Fatal("partial success reset backoff or ran past rate limit", calls)
		}
		list, err := monitors(a.store.DB)
		if err != nil || list[0].NextRun < until || list[1].NextRun < until || list[1].CatchupScrolls != 20 {
			t.Fatal("rate-limited batch did not preserve its schedule/checkpoint", list, err)
		}
		callsBeforeRestart := calls
		// Recreate the app against durable settings/state, as on a restart.
		restarted, err := New(a.configPath, a.Config(), a.store, false)
		if err != nil {
			t.Fatal(err)
		}
		restarted.scheduler.download = a.scheduler.download
		restarted.scheduler.avatar = a.scheduler.avatar
		a = restarted
		a.scheduler.collect = collector
		if err := a.scheduler.queue("carol"); err != nil {
			t.Fatal(err)
		}
		if worked, err := a.scheduler.step(context.Background()); worked || err != nil || calls != callsBeforeRestart {
			t.Fatal("restart/manual queue bypassed cooldown", worked, err, calls)
		}
		// Advance simulated scheduling time without sleeping or contacting X.
		if err := setState(a.store.DB, "cooldown_until", "0"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=0,requested=0"); err != nil {
			t.Fatal(err)
		}
	}
	// Only a whole successful automatic batch clears the counter.
	failBob = false
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if state(a.store.DB, "rate_limit_failures") != "0" || state(a.store.DB, "cooldown_until") != "0" {
		t.Fatal("successful batch did not reset backoff")
	}
}

func TestSchedulerManualSuccessDoesNotResetRateBackoff(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := setState(a.store.DB, "rate_limit_failures", "3"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=?", time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if err := a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if state(a.store.DB, "rate_limit_failures") != "3" {
		t.Fatal("manual success cleared shared backoff")
	}
}

func TestSchedulerRateLimitHonorsUpstreamDeadline(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	retryAt := time.Now().Add(12 * time.Hour).Truncate(time.Second)
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		return archive.Collection{}, &archive.Failure{Kind: "rate", Message: "synthetic", RetryAt: retryAt}
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if got := state(a.store.DB, "cooldown_until"); got != strconv.FormatInt(retryAt.Unix()+5, 10) {
		t.Fatal("upstream reset lost", got)
	}
}

func TestSchedulerCollectionErrorsDoNotIncreaseCatchupBudget(t *testing.T) {
	for _, kind := range []string{"rate", "auth", "transient"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
				return archive.Collection{}, &archive.Failure{Kind: kind, Message: "synthetic"}
			}
			if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
				t.Fatal(worked, err)
			}
			list, err := monitors(a.store.DB)
			if err != nil || !list[0].Catchup || list[0].CatchupScrolls != 0 {
				t.Fatal("failed collection expanded pagination", list, err)
			}
		})
	}
}

func TestUpgradeActiveLegacyRateLimit(t *testing.T) {
	a := testApp(t)
	oldUntil := time.Now().Unix() + 600 // The old rate failure was five minutes ago.
	if err := setState(a.store.DB, "cooldown_until", strconv.FormatInt(oldUntil, 10)); err != nil {
		t.Fatal(err)
	}
	cfg := a.Config()
	cfg.DefaultInterval = 3600
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := New(a.configPath, cfg, a.store, false); err != nil {
			t.Fatal(err)
		}
		if got := state(a.store.DB, "cooldown_until"); got != strconv.FormatInt(oldUntil-900+3600, 10) {
			t.Fatal("upgrade failed to extend existing cooldown, or extended it again on restart", got)
		}
		if state(a.store.DB, "rate_limit_failures") != "1" {
			t.Fatal("legacy cooldown not counted")
		}
	}
}

func TestUpgradeIgnoresExpiredLegacyRateLimit(t *testing.T) {
	a := testApp(t)
	if err := setState(a.store.DB, "cooldown_until", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(a.configPath, a.Config(), a.store, false); err != nil {
		t.Fatal(err)
	}
	if state(a.store.DB, "rate_limit_failures") != "" || state(a.store.DB, "cooldown_until") != "1" {
		t.Fatal("revived an expired cooldown")
	}
}

func TestSchedulerRateStateRollsBackWithRun(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite failure injection")
	}
	seedMonitor(t, a, "alice")
	if _, err := a.store.DB.Exec(`CREATE TRIGGER reject_rate_finish BEFORE UPDATE ON runs WHEN NEW.status='failed' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		return archive.Collection{}, &archive.Failure{Kind: "rate", Message: "synthetic"}
	}
	if _, err := a.scheduler.step(context.Background()); err == nil {
		t.Fatal("ignored failed finalization")
	}
	if state(a.store.DB, "rate_limit_failures") != "" || state(a.store.DB, "cooldown_until") != "" {
		t.Fatal("rate state committed without run/checkpoint")
	}
}
