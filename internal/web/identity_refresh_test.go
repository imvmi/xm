package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"xm/internal/archive"
)

func identityRefreshFixture(t *testing.T) *App {
	t.Helper()
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "123", AuthorID: "42", Username: "alice"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0,next_run=123,last_error='目标账号不可用',interval_override_seconds=300,show_in_timeline=0"); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRefreshMonitorIdentityPreservesArchiveAndSettings(t *testing.T) {
	a := identityRefreshFixture(t)
	b := login(t, a)
	before, err := monitors(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	a.refreshIdentity = func(_ context.Context, _ archive.Config, _ archive.Cookies, i archive.TargetIdentity, refs []string, name string) (archive.Profile, error) {
		if i.UserID != "42" || !reflect.DeepEqual(refs, []string{"123"}) || name != "" {
			t.Fatal(i, refs, name)
		}
		if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
			t.Fatal("scheduler overlapped lookup", worked, err)
		}
		ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true), 409)
		return archive.Profile{ID: "42", Username: "newalice"}, nil
	}
	w := b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true)
	ok(t, w, 200)
	if !strings.Contains(w.Body.String(), "自动采集仍暂停") {
		t.Fatal(w.Body.String())
	}
	after, err := monitors(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	before[0].CurrentUsername = "newalice"
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refresh changed collection or archive", before, after)
	}
	a.refreshIdentity = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ archive.TargetIdentity, _ []string, name string) (archive.Profile, error) {
		if name != "newalice" {
			t.Fatal("manual URL not normalized", name)
		}
		return archive.Profile{ID: "42", Username: name}, nil
	}
	ok(t, b.request("POST", "/api/monitors/alice/identity", map[string]string{"username": "https://x.com/newalice"}, "http://localhost", true), 200)
	if a.scheduler.active {
		t.Fatal("lookup did not release scheduler")
	}
}

func TestRefreshMonitorIdentityRejectsWrongAccountAndInvalidRequests(t *testing.T) {
	a := identityRefreshFixture(t)
	b := login(t, a)
	calls := 0
	a.refreshIdentity = func(context.Context, archive.Config, archive.Cookies, archive.TargetIdentity, []string, string) (archive.Profile, error) {
		calls++
		return archive.Profile{ID: "99", Username: "other"}, nil
	}
	ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", false), 403)
	ok(t, b.request("POST", "/api/monitors/alice/identity", map[string]string{"username": "https://example.com/other"}, "http://localhost", true), 400)
	ok(t, b.request("POST", "/api/monitors/missing/identity", nil, "http://localhost", true), 404)
	if calls != 0 {
		t.Fatal("invalid request reached resolver")
	}
	ok(t, b.request("POST", "/api/monitors/alice/identity", map[string]string{"username": "other"}, "http://localhost", true), 409)
	i, err := a.store.TargetIdentity("alice")
	if err != nil || i.UserID != "42" || i.CurrentUsername != "alice" {
		t.Fatal(i, err)
	}
	seedMonitor(t, a, "unbound")
	ok(t, b.request("POST", "/api/monitors/unbound/identity", nil, "http://localhost", true), 409)
	if calls != 1 {
		t.Fatal("unbound account reached resolver")
	}
}

func TestRefreshMonitorIdentityGatesAndFailure(t *testing.T) {
	for _, kind := range []string{"rate", "auth", "transient"} {
		t.Run(kind, func(t *testing.T) {
			a := identityRefreshFixture(t)
			b := login(t, a)
			a.refreshIdentity = func(context.Context, archive.Config, archive.Cookies, archive.TargetIdentity, []string, string) (archive.Profile, error) {
				return archive.Profile{}, &archive.Failure{Kind: kind, Message: "fixture failure"}
			}
			want := 502
			if kind == "rate" {
				want = 429
			}
			ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true), want)
			gates, err := loadSchedulerState(a.store.DB)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "rate" && (gates.CooldownUntil <= time.Now().Unix() || gates.RateFailures != 1) {
				t.Fatal("cooldown not saved", gates)
			}
			if kind == "auth" && gates.CookieBlocked == "" {
				t.Fatal("auth failure not saved")
			}
			if kind != "transient" {
				a.refreshIdentity = func(context.Context, archive.Config, archive.Cookies, archive.TargetIdentity, []string, string) (archive.Profile, error) {
					t.Fatal("blocked lookup executed")
					return archive.Profile{}, nil
				}
				want = 409
				if kind == "rate" {
					want = 429
				}
				ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true), want)
			}
			i, err := a.store.TargetIdentity("alice")
			if err != nil || i.CurrentUsername != "alice" {
				t.Fatal(i, err)
			}
		})
	}
}

func TestRefreshMonitorIdentityCancellationAndRemoval(t *testing.T) {
	for _, removed := range []bool{false, true} {
		a := identityRefreshFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		a.refreshIdentity = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ archive.TargetIdentity, _ []string, _ string) (archive.Profile, error) {
			if removed {
				if _, err := a.store.DB.Exec("DELETE FROM monitors WHERE username='alice'"); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			return archive.Profile{ID: "42", Username: "newalice"}, nil
		}
		req := httptest.NewRequest("POST", "/api/monitors/alice/identity", strings.NewReader(`{}`)).WithContext(ctx)
		req.SetPathValue("username", "alice")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.updateMonitorIdentity(w, req)
		cancel()
		want := 408
		if removed {
			want = 404
		}
		ok(t, w, want)
		i, err := a.store.TargetIdentity("alice")
		if err != nil || i.CurrentUsername != "alice" {
			t.Fatal(i, err)
		}
		if a.scheduler.active {
			t.Fatal("reservation leaked")
		}
	}
}

func TestRefreshMonitorIdentityDoesNotBlockNewCookies(t *testing.T) {
	a := identityRefreshFixture(t)
	b := login(t, a)
	a.refreshIdentity = func(context.Context, archive.Config, archive.Cookies, archive.TargetIdentity, []string, string) (archive.Profile, error) {
		if err := setState(a.store.DB, "cookie_imported", "replacement"); err != nil {
			t.Fatal(err)
		}
		return archive.Profile{}, &archive.Failure{Kind: "auth", Message: "old cookies failed"}
	}
	ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true), 502)
	if state(a.store.DB, "cookie_blocked") != "" {
		t.Fatal("old failure blocked new cookie")
	}
	a.refreshIdentity = func(context.Context, archive.Config, archive.Cookies, archive.TargetIdentity, []string, string) (archive.Profile, error) {
		return archive.Profile{}, errors.New("unavailable")
	}
	ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true), 502)
}

func TestRefreshMonitorIdentityRetainsUnpersistedCooldown(t *testing.T) {
	a := identityRefreshFixture(t)
	b := login(t, a)
	trigger := "CREATE TRIGGER reject_identity_cooldown BEFORE INSERT ON web_state WHEN NEW.key='cooldown_until' BEGIN SELECT RAISE(ABORT,'injected failure'); END"
	if a.store.DB.Dialect == "mysql" {
		trigger = "CREATE TRIGGER reject_identity_cooldown BEFORE INSERT ON web_state FOR EACH ROW BEGIN IF NEW.`key`='cooldown_until' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected failure'; END IF; END"
	}
	if _, err := a.store.DB.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	a.refreshIdentity = func(context.Context, archive.Config, archive.Cookies, archive.TargetIdentity, []string, string) (archive.Profile, error) {
		return archive.Profile{}, &archive.Failure{Kind: "rate", Message: "rate limit"}
	}
	ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true), 500)
	if a.identityFailure == nil {
		t.Fatal("lost unpersisted rate limit")
	}
	if worked, err := a.scheduler.step(context.Background()); worked || err == nil {
		t.Fatal("scheduler ignored pending failure", worked, err)
	}
	ok(t, b.request("POST", "/api/monitors/alice/identity", nil, "http://localhost", true), 409)
	if _, err := a.store.DB.Exec("DROP TRIGGER reject_identity_cooldown"); err != nil {
		t.Fatal(err)
	}
	if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
		t.Fatal(worked, err)
	}
	gates, err := loadSchedulerState(a.store.DB)
	if err != nil || gates.CooldownUntil <= time.Now().Unix() || a.identityFailure != nil {
		t.Fatal("cooldown not recovered", gates, err)
	}
}
