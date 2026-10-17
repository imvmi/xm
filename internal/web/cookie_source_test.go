package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"xm/internal/archive"
)

func TestCookieSourceAPI(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	body := `{"auth_token":"new-auth","ct0":"new-csrf"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer source-secret" {
			t.Error("missing source credential")
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	input := map[string]any{"url": srv.URL, "token": "Bearer source-secret", "auto_refresh": true}
	ok(t, b.request("PUT", "/api/cookies/source", input, "http://localhost", false), 403)
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/api/cookies/source", nil, "", false), 401)
	ok(t, b.request("PUT", "/api/cookies/source", input, "http://localhost", true), 200)
	ok(t, b.request("POST", "/api/cookies/fetch", nil, "http://localhost", false), 403)
	ok(t, b.request("POST", "/api/cookies/fetch", nil, "http://localhost", true), 200)
	c, err := a.store.LoadCookies()
	if err != nil || c.AuthToken != "new-auth" {
		t.Fatal("not imported", err)
	}
	response := b.request("GET", "/api/cookies/source", nil, "", false)
	ok(t, response, 200)
	if strings.Contains(response.Body.String(), "source-secret") || strings.Contains(response.Body.String(), "new-auth") {
		t.Fatal("credentials leaked")
	}
	delete(input, "token")
	ok(t, b.request("PUT", "/api/cookies/source", input, "http://localhost", true), 200)
	source, _ := a.store.LoadCookieSource()
	if source.BearerToken != "source-secret" {
		t.Fatal("empty token did not preserve credential")
	}
	body = `{"auth_token":"incomplete"}`
	ok(t, b.request("POST", "/api/cookies/fetch", nil, "http://localhost", true), 502)
	after, _ := a.store.LoadCookies()
	if after != c {
		t.Fatal("failed fetch overwrote cookies")
	}
	input["url"] = "https://example.com/different"
	ok(t, b.request("PUT", "/api/cookies/source", input, "http://localhost", true), 200)
	source, _ = a.store.LoadCookieSource()
	if source.BearerToken != "" {
		t.Fatal("credential retained for different destination")
	}
	input["token"] = "another-secret"
	ok(t, b.request("PUT", "/api/cookies/source", input, "http://localhost", true), 200)
	delete(input, "token")
	input["clear_token"] = true
	ok(t, b.request("PUT", "/api/cookies/source", input, "http://localhost", true), 200)
	source, _ = a.store.LoadCookieSource()
	if source.BearerToken != "" {
		t.Fatal("clear token ignored")
	}
}

func TestCookieAPIAutomaticRecovery(t *testing.T) {
	for _, mode := range []string{"success", "still-invalid", "api-failed", "disabled", "transient", "access", "rate"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			fetches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fetches++
				if mode == "api-failed" {
					w.WriteHeader(503)
					return
				}
				w.Write([]byte(`{"auth_token":"fresh","ct0":"fresh-csrf"}`))
			}))
			defer srv.Close()
			if err := a.store.SaveCookieSource(archive.CookieSource{URL: srv.URL, AutoRefresh: mode != "disabled"}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			a.scheduler.collect = func(_ context.Context, _ archive.Config, c archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
				calls++
				if mode == "success" && c.AuthToken == "fresh" {
					return archive.Collection{Stop: "known_boundary_reached"}, nil
				}
				kind := "auth"
				if mode == "transient" || mode == "access" || mode == "rate" {
					kind = mode
				}
				return archive.Collection{}, &archive.Failure{Kind: kind, Message: "fixture collection failure"}
			}
			_, err := a.scheduler.step(context.Background())
			if (err != nil) != (mode == "api-failed") {
				t.Fatal("unexpected recovery error", err)
			}
			expectedFetch := mode == "success" || mode == "still-invalid" || mode == "api-failed"
			if (fetches == 1) != expectedFetch {
				t.Fatal("wrong recovery trigger", fetches)
			}
			if mode == "success" || mode == "still-invalid" {
				if _, err = a.scheduler.step(context.Background()); err != nil {
					t.Fatal(err)
				}
				if calls != 2 {
					t.Fatal("failed target not retried", calls)
				}
				if mode == "success" {
					if state(a.store.DB, "cookie_blocked") != "" || state(a.store.DB, "cookie_api_attempted") != "" {
						t.Fatal("success did not rearm recovery")
					}
				} else {
					if state(a.store.DB, "cookie_blocked") == "" {
						t.Fatal("invalid replacement not blocked")
					}
					a.scheduler = newScheduler(a)
					if worked, err := a.scheduler.step(context.Background()); worked || err != nil || fetches != 1 {
						t.Fatal("restart retried invalid cookies")
					}
				}
			}
			if mode == "api-failed" {
				c, _ := a.store.LoadCookies()
				if c.AuthToken != "synthetic" {
					t.Fatal("old cookie lost")
				}
				if _, err = a.scheduler.step(context.Background()); err != nil || fetches != 1 {
					t.Fatal("retried failed API")
				}
			}
		})
	}
}

func TestCookieAPIManualImportAndAtomicFailure(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := setState(a.store.DB, "cookie_api_attempted", "1"); err != nil {
		t.Fatal(err)
	}
	if err := setState(a.store.DB, "cookie_blocked", "expired"); err != nil {
		t.Fatal(err)
	}
	b := login(t, a)
	ok(t, b.request("POST", "/api/cookies", map[string]string{"content": `{"auth_token":"manual","ct0":"manual-csrf"}`}, "http://localhost", true), 200)
	if state(a.store.DB, "cookie_api_attempted") != "" || state(a.store.DB, "cookie_blocked") != "" {
		t.Fatal("manual import did not rearm recovery")
	}
	if a.store.DB.Dialect != "sqlite" {
		return
	}
	before, _ := a.store.LoadCookies()
	if _, err := a.store.DB.Exec(`CREATE TRIGGER reject_cookie_state BEFORE INSERT ON web_state WHEN NEW.key='cookie_imported' BEGIN SELECT RAISE(ABORT,'fixture error'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.saveImportedCookies(archive.Cookies{AuthToken: "replacement", CSRF: "replacement-csrf"}, true, true); err == nil {
		t.Fatal("ignored state write failure")
	}
	after, _ := a.store.LoadCookies()
	if after != before {
		t.Fatal("cookie replacement was not rolled back with state")
	}
}
