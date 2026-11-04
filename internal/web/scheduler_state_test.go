package web

import (
	"context"
	"fmt"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestSchedulerCompletionWriteFailurePreservesUpstreamStop(t *testing.T) {
	for _, tc := range []struct{ name, kind, trigger string }{
		{"cooldown", "rate", "BEFORE INSERT ON web_state WHEN NEW.key='cooldown_until'"},
		{"cookie_block", "auth", "BEFORE INSERT ON web_state WHEN NEW.key='cookie_blocked'"},
		{"disable_access", "access", "BEFORE UPDATE ON monitors WHEN NEW.enabled=0"},
		{"disable_identity", "identity", "BEFORE UPDATE ON monitors WHEN NEW.enabled=0"},
		{"disable_checkpoint", "checkpoint", "BEFORE UPDATE ON monitors WHEN NEW.enabled=0"},
		{"rate_run_completion", "rate", "BEFORE UPDATE ON runs WHEN NEW.status='failed'"},
		{"rate_monitor_completion", "rate", "BEFORE UPDATE ON monitors WHEN NEW.running=0"},
		{"auth_run_completion", "auth", "BEFORE UPDATE ON runs WHEN NEW.status='failed'"},
		// This fails after completion committed: recovery must not count the
		// already-persisted rate limit a second time.
		{"rate_notification", "rate", "BEFORE INSERT ON notification_incidents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			if a.store.DB.Dialect != "sqlite" {
				t.Skip("SQLite failure injection")
			}
			seedMonitor(t, a, "alice")
			if err := setState(a.store.DB, "rate_limit_failures", "2"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			retryAt := time.Now().Add(time.Hour)
			a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
				calls++
				if calls == 1 {
					_, err := a.store.DB.Exec(fmt.Sprintf("CREATE TRIGGER reject_completion_write %s BEGIN SELECT RAISE(ABORT, 'injected write failure'); END", tc.trigger))
					if err != nil {
						t.Fatal(err)
					}
				}
				return archive.Collection{}, &archive.Failure{Kind: tc.kind, Message: "upstream stop", RetryAt: retryAt}
			}
			if _, err := a.scheduler.step(context.Background()); err == nil {
				t.Fatal("completion write failure ignored")
			}
			// Even when the database is still failing, recovery must not collect.
			_, _ = a.scheduler.step(context.Background())
			if calls != 1 {
				t.Fatal("collected before persistence recovered", calls)
			}
			if _, err := a.store.DB.Exec("DROP TRIGGER reject_completion_write"); err != nil {
				t.Fatal(err)
			}
			if _, err := a.scheduler.step(context.Background()); err != nil {
				t.Fatal("recovery failed", err)
			}
			if calls != 1 {
				t.Fatal("upstream stop forgotten", calls)
			}
			gates, err := loadSchedulerState(a.store.DB)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.kind {
			case "rate":
				if gates.CooldownUntil < retryAt.Unix() || gates.RateFailures != 3 {
					t.Fatal("cooldown/backoff lost or applied twice", gates)
				}
			case "auth":
				if gates.CookieBlocked != "upstream stop" {
					t.Fatal("cookie block lost", gates)
				}
			default:
				var enabled bool
				if err := a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username='alice'").Scan(&enabled); err != nil || enabled {
					t.Fatal("account stop lost", enabled, err)
				}
			}
			if a.scheduler.recovery != nil {
				t.Fatal("recovery not completed")
			}
		})
	}
}

func TestSchedulerStateFailuresPreserveQueue(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.scheduler.queue("alice"); err != nil {
		t.Fatal(err)
	}
	if err := setState(a.store.DB, "budget_batch_pending", `["alice"]`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		calls++
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	// Fault only state reads: archive, cookies and the durable queue remain usable.
	if _, err := a.store.DB.Exec("ALTER TABLE web_state RENAME TO unavailable_state"); err != nil {
		t.Fatal(err)
	}
	if worked, err := a.scheduler.step(context.Background()); err == nil || worked {
		t.Fatalf("read failure accepted: %v %v", worked, err)
	}
	if err := a.scheduler.queue("alice"); err == nil {
		t.Fatal("manual enqueue ignored state failure")
	}
	if err := a.recoverCookie(context.Background(), "", "alice"); err == nil {
		t.Fatal("cookie recovery ignored state failure")
	}
	if err := upgradeRateLimitState(a.store.DB, 3600, "balanced", time.Now().Unix()); err == nil {
		t.Fatal("startup upgrade ignored state failure")
	}
	var status string
	if err := a.store.DB.QueryRow("SELECT status FROM collection_jobs WHERE target='alice'").Scan(&status); err != nil || status != "queued" || calls != 0 {
		t.Fatal(status, calls, err)
	}
	if _, err := a.store.DB.Exec("ALTER TABLE unavailable_state RENAME TO web_state"); err != nil {
		t.Fatal(err)
	}
	if state(a.store.DB, "budget_batch_pending") != `["alice"]` {
		t.Fatal("pending batch changed")
	}
	if worked, err := a.scheduler.step(context.Background()); err != nil || !worked || calls != 1 {
		t.Fatal("queue did not recover", worked, calls, err)
	}
}

func TestSchedulerStateRejectsMalformedGates(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	for _, tc := range []struct{ key, value string }{
		{"cooldown_until", "garbage"}, {"cooldown_until", "-1"}, {"cooldown_until", "9223372036854775808"},
		{"rate_limit_failures", "-2"}, {"rate_limit_failures", "NaN"}, {"budget_batch_failed", "yes"},
		{"cookie_api_attempted", "yes"}, {"budget_batch_pending", `{"alice":true}`},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			if err := setState(a.store.DB, tc.key, tc.value); err != nil {
				t.Fatal(err)
			}
			if worked, err := a.scheduler.step(context.Background()); err == nil || worked {
				t.Fatal("corrupt gate ignored", worked, err)
			}
			if err := setState(a.store.DB, tc.key, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := loadSchedulerState(a.store.DB); err != nil {
		t.Fatal("legacy empty defaults rejected", err)
	}
	if value, err := readState(a.store.DB, "absent"); err != nil || value != "" {
		t.Fatal(value, err)
	}
	var count int
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM collection_jobs").Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid states changed queue", count, err)
	}
}

func TestSchedulerRechecksCooldownAtTaskBoundary(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	calls := 0
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		calls++
		return archive.Collection{Stop: "known_boundary_reached"}, setState(a.store.DB, "cooldown_until", "4102444800")
	}
	if _, err := a.scheduler.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || state(a.store.DB, "cooldown_until") != "4102444800" || state(a.store.DB, "budget_batch_pending") != `["bob"]` {
		t.Fatal("boundary gate was bypassed", calls, state(a.store.DB, "budget_batch_pending"))
	}
}

func TestSchedulerStateFailureAfterCollectionRecoversWithoutRestart(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	calls := 0
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		calls++
		if err := save(archive.Page{Posts: []archive.Post{{ID: "100", AuthorID: "42", Username: "alice"}}}); err != nil {
			return archive.Collection{}, err
		}
		if calls == 1 {
			_, err := a.store.DB.Exec("ALTER TABLE web_state RENAME TO unavailable_state")
			if err != nil {
				t.Fatal(err)
			}
		}
		return archive.Collection{Posts: 1, Stop: "known_boundary_reached"}, nil
	}
	if _, err := a.scheduler.step(context.Background()); err == nil {
		t.Fatal("completion ignored read failure")
	}
	if _, err := a.store.DB.Exec("ALTER TABLE unavailable_state RENAME TO web_state"); err != nil {
		t.Fatal(err)
	}
	if worked, err := a.scheduler.step(context.Background()); err != nil || !worked || calls != 2 {
		t.Fatal("interrupted attempt stranded", worked, calls, err)
	}
	var count, running int
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM posts WHERE id='100'").Scan(&count); err != nil || count != 1 {
		t.Fatal("archive lost or duplicated", count, err)
	}
	if err := a.store.DB.QueryRow("SELECT running FROM monitors WHERE username='alice'").Scan(&running); err != nil || running != 0 {
		t.Fatal("monitor still running", running, err)
	}
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE status='interrupted'").Scan(&count); err != nil || count != 1 {
		t.Fatal("interrupted run not recorded", count, err)
	}
}

func TestSchedulerStateRecoveryPreservesUpstreamStop(t *testing.T) {
	for _, kind := range []string{"rate", "auth", "access"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			if err := setState(a.store.DB, "rate_limit_failures", "2"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
				calls++
				if _, err := a.store.DB.Exec("ALTER TABLE web_state RENAME TO unavailable_state"); err != nil {
					t.Fatal(err)
				}
				return archive.Collection{}, &archive.Failure{Kind: kind, Message: "upstream stop"}
			}
			if _, err := a.scheduler.step(context.Background()); err == nil {
				t.Fatal("completion read failure ignored")
			}
			if _, err := a.store.DB.Exec("ALTER TABLE unavailable_state RENAME TO web_state"); err != nil {
				t.Fatal(err)
			}
			if _, err := a.scheduler.step(context.Background()); err != nil {
				t.Fatal("recovery failed", err)
			}
			if calls != 1 {
				t.Fatal("upstream stop forgotten")
			}
			gates, err := loadSchedulerState(a.store.DB)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "rate" && (gates.CooldownUntil <= time.Now().Unix() || gates.RateFailures != 3) {
				t.Fatal("cooldown/backoff lost", gates)
			}
			if kind == "auth" && gates.CookieBlocked != "upstream stop" {
				t.Fatal("cookie block lost", gates)
			}
			if kind == "access" {
				var enabled int
				if err := a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username='alice'").Scan(&enabled); err != nil || enabled != 0 {
					t.Fatal("access block lost", enabled, err)
				}
			}
		})
	}
}
