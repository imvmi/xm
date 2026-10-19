package archive

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

// This launches real Chrome but intercepts every page request before networking.
// It uses synthetic cookies and fixtures, and never contacts X.
func TestBrowserCollectionOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("set XM_BROWSER_TEST=1 to run real-Chrome fixture integration")
	}
	retryAt := time.Now().UTC().Add(12 * time.Hour).Truncate(time.Second)
	for _, test := range []struct {
		name, body, operation string
		wantErr               bool
		status                int64
		retryHeader           string
	}{
		{"authorized", string(fixture(t, "timeline.json")), "UserTweets", false, 200, ""},
		{"originals_timeline", string(fixture(t, "timeline.json")), "UserOriginalsTimeline", false, 200, ""},
		{"replies_timeline", string(fixture(t, "timeline.json")), "UserRepliesTimeline", false, 200, ""},
		{"protected_denied", `{"errors":[{"code":179}]}`, "UserOriginalsTimeline", true, 200, ""},
		{"expired_cookie", `{"errors":[{"code":89}]}`, "UserOriginalsTimeline", true, 200, ""},
		{"rate_limited", `{"errors":[{"code":88}]}`, "UserOriginalsTimeline", true, 200, "x-rate-limit-reset"},
		{"http_429", "", "UserOriginalsTimeline", true, 429, "Retry-After"},
		{"http_503", "", "UserOriginalsTimeline", true, 503, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			cfg := Config{DataDir: dir, Headless: true, BrowserMaxAge: 20, ScrollDelay: 1, MaxScrolls: 3}
			setup := func(ctx context.Context) error {
				err := chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
					_, e := cdp.Call(c, target, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}})
					return e
				}))
				if err != nil {
					return err
				}
				events := chromedp.Events(ctx, fetch.RequestPaused)
				go func() {
					for e, err := range events {
						if err != nil {
							return
						}
						body, ct := "", "text/html"
						status := int64(200)
						headers := []*fetch.HeaderEntry{}
						switch {
						case strings.Contains(e.Request.URL, "/UserByRestId"):
							ct = "application/json"
							status = 403
						case strings.Contains(e.Request.URL, "/UserByScreenName"):
							ct = "application/json"
							body = `{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"alice"},"privacy":{"protected":true}}}}}`
						case strings.HasSuffix(e.Request.URL, "/"+test.operation):
							ct = "application/json"
							body = test.body
							status = test.status
							if test.retryHeader == "Retry-After" {
								headers = append(headers, &fetch.HeaderEntry{Name: test.retryHeader, Value: retryAt.Format("Mon, 02 Jan 2006 15:04:05 GMT")})
							} else if test.retryHeader != "" {
								headers = append(headers, &fetch.HeaderEntry{Name: test.retryHeader, Value: strconv.FormatInt(retryAt.Unix(), 10)})
							}
						case strings.HasSuffix(e.Request.URL, "/alice") || strings.HasSuffix(e.Request.URL, "/alice/with_replies"):
							noise := ""
							if !test.wantErr {
								noise = `await fetch('/i/api/graphql/fixture/UserByRestId?variables=%7B%22userId%22%3A%2299%22%7D');`
							}
							body = `<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>(async()=>{` + noise + `await fetch('/i/api/graphql/fixture/UserByScreenName');await new Promise(r=>setTimeout(r,300));await fetch('/i/api/graphql/fixture/` + test.operation + `');})()</script></body></html>`
						}
						headers = append(headers, &fetch.HeaderEntry{Name: "Content-Type", Value: ct})
						_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
							_, err := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: status, ResponseHeaders: headers, Body: []byte(body)})
							return err
						}))
					}
				}()
				return nil
			}
			identity := TargetIdentity{}
			if !test.wantErr {
				identity = TargetIdentity{UserID: "42", CurrentUsername: "alice"}
			}
			result, err := collectIdentity(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 2, test.operation == "UserRepliesTimeline", func(p Page) error {
				if p.Profile != nil {
					if err := s.SaveProfile(*p.Profile); err != nil {
						return err
					}
				}
				return s.SavePosts("alice", p.Posts)
			}, setup)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected explicit failure")
				}
				if test.name == "http_503" && FailureKind(err) != "timeline" {
					t.Fatalf("temporary upstream failure was not retryable: %v", err)
				}
				if test.retryHeader != "" {
					var failure *Failure
					if !errors.As(err, &failure) || failure.Kind != "rate" || !failure.RetryAt.Equal(retryAt) || !strings.Contains(failure.Message, test.operation) {
						t.Fatalf("lost rate-limit deadline/operation: %#v", err)
					}
				}
				var n int
				s.DB.QueryRow("SELECT count(*) FROM posts").Scan(&n)
				if n != 0 {
					t.Fatal("failed auth stored posts")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Posts != 2 || result.Stop != "limit_reached" {
				t.Fatalf("wrong result %+v", result)
			}
			var n int
			s.DB.QueryRow("SELECT count(*) FROM posts").Scan(&n)
			if n != 2 {
				t.Fatal("browser data not saved")
			}
		})
	}
}

func TestRequestSubjectAndRelevance(t *testing.T) {
	meta := responseMeta{operation: "UserByRestId", userID: "42"}
	userID, username, postID := requestSubject("https://x.com/i/api/graphql/query/UserByRestId?variables=%7B%22userId%22%3A%2242%22%2C%22screen_name%22%3A%22Alice%22%2C%22focalTweetId%22%3A%22123%22%7D")
	if userID != "42" || username != "Alice" || postID != "123" {
		t.Fatal(userID, username, postID)
	}
	if !relevantResponse(meta, "alice", "42", "", "alice", "123", false) {
		t.Fatal("target lookup was discarded")
	}
	meta.userID = "99"
	if relevantResponse(meta, "alice", "42", "", "alice", "123", false) {
		t.Fatal("unrelated user lookup was accepted")
	}
	if relevantResponse(responseMeta{username: "bob"}, "alice", "", "", "", "", false) {
		t.Fatal("unrelated username lookup was accepted")
	}
	if relevantResponse(responseMeta{postID: "456"}, "alice", "42", "", "alice", "123", true) {
		t.Fatal("unrelated post lookup was accepted")
	}
}

func TestIdentityRetryWithoutPostAnchorKeepsHandleRoute(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixture")
	}
	cfg := Config{Headless: true, BrowserMaxAge: 30, ScrollDelay: 1, TimelineIdleSeconds: 3}
	visits := 0
	usedRestIDRoute := false
	setup := func(ctx context.Context) error {
		if err := chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
			_, err := cdp.Call(c, target, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}})
			return err
		})); err != nil {
			return err
		}
		events := chromedp.Events(ctx, fetch.RequestPaused)
		go func() {
			for e, eventErr := range events {
				if eventErr != nil {
					return
				}
				body, contentType, status := "", "text/html", int64(200)
				switch {
				case strings.Contains(e.Request.URL, "/UserByScreenName"):
					body, contentType = `{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"alice"}}}}}`, "application/json"
				case strings.Contains(e.Request.URL, "/UserTweets"):
					body, contentType = string(fixture(t, "timeline.json")), "application/json"
				case strings.Contains(e.Request.URL, "/UserByRestId"):
					status, contentType = 403, "application/json"
				case strings.HasSuffix(e.Request.URL, "/alice"):
					visits++
					requests := `fetch('/i/api/graphql/fixture/UserByScreenName')`
					if visits > 1 {
						requests += `;fetch('/i/api/graphql/fixture/UserTweets?variables=%7B%22userId%22%3A%2242%22%7D')`
					}
					body = `<button data-testid="SideNav_AccountSwitcher_Button">Account</button><div style="height:2000px"></div><script>` + requests + `</script>`
				case strings.Contains(e.Request.URL, "/i/user/42"):
					usedRestIDRoute = true
					body = `<button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>fetch('/i/api/graphql/fixture/UserByRestId?variables=%7B%22userId%22%3A%2242%22%7D')</script>`
				}
				_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
					_, err := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: status, ResponseHeaders: []*fetch.HeaderEntry{{Name: "Content-Type", Value: contentType}}, Body: []byte(body)})
					return err
				}))
			}
		}()
		return nil
	}
	result, err := collectIdentity(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", TargetIdentity{UserID: "42", CurrentUsername: "alice"}, 2, false, func(Page) error { return nil }, setup)
	if err != nil || result.Posts != 2 || visits != 2 || usedRestIDRoute {
		t.Fatal(result, err, visits, usedRestIDRoute)
	}
}

func TestBrowserSessionCollectsMultipleTargetsOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("set XM_BROWSER_TEST=1 to run real-Chrome fixture integration")
	}
	cfg := Config{Headless: true, BrowserMaxAge: 20, ScrollDelay: 1, MaxScrolls: 3}
	session, err := NewBrowserSession(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	process := chromedp.FromContext(session.root).Browser.Process()
	timeline := string(fixture(t, "timeline.json"))
	for _, username := range []string{"alice", "bob"} {
		setup := func(ctx context.Context) error {
			if err := chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
				_, e := cdp.Call(c, target, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}})
				return e
			})); err != nil {
				return err
			}
			events := chromedp.Events(ctx, fetch.RequestPaused)
			go func() {
				for e, eventErr := range events {
					if eventErr != nil {
						return
					}
					body, contentType := "", "text/html"
					switch {
					case strings.Contains(e.Request.URL, "/UserByScreenName"):
						body, contentType = `{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"`+username+`"}}}}}`, "application/json"
					case strings.Contains(e.Request.URL, "/UserTweets"):
						body, contentType = timeline, "application/json"
					case strings.HasSuffix(e.Request.URL, "/"+username):
						body = `<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>(async()=>{await fetch('/i/api/graphql/fixture/UserByScreenName');await fetch('/i/api/graphql/fixture/UserTweets')})()</script></body></html>`
					}
					_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
						_, callErr := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: 200, ResponseHeaders: []*fetch.HeaderEntry{{Name: "Content-Type", Value: contentType}}, Body: []byte(body)})
						return callErr
					}))
				}
			}()
			return nil
		}
		job, cancelJob := context.WithCancel(context.Background())
		result, collectErr := session.collectBound(job, username, TargetIdentity{}, 2, false, func(Page) error { return nil }, setup)
		cancelJob()
		if collectErr != nil || result.Posts != 2 || result.Stop != "limit_reached" {
			t.Fatalf("shared session failed for %s: %+v err=%v", username, result, collectErr)
		}
		if err := session.Healthy(context.Background()); err != nil || chromedp.FromContext(session.root).Browser.Process() != process {
			t.Fatal("job cancellation stopped or replaced root browser", err)
		}
		targets, err := chromedp.Targets(session.root)
		if err != nil {
			t.Fatal(err)
		}
		pages := 0
		for _, target := range targets {
			if target.Type == "page" {
				pages++
				if target.URL != "about:blank" {
					t.Fatal("task tab leaked while idle", target.URL)
				}
			}
		}
		if pages != 1 {
			t.Fatal("expected only the blank root tab", pages)
		}
	}
}

func TestBrowserSessionTimeSliceAndCrashOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome lifecycle fixture")
	}
	cfg := DefaultConfig()
	cfg.BrowserMaxAge = 1
	session, err := NewBrowserSession(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	blockedSetup := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	result, err := session.collectBound(context.Background(), "alice", TargetIdentity{DurableProgress: true, Catchup: true, CatchupScrollBudget: 1000}, 100, false, func(Page) error { return nil }, blockedSetup)
	if err != nil || result.Stop != "time_budget_reached" {
		t.Fatal("time slice became a failure", result, err)
	}
	if err = session.Healthy(context.Background()); err != nil {
		t.Fatal("time slice closed shared browser", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = session.collectBound(ctx, "alice", TargetIdentity{DurableProgress: true}, 100, false, func(Page) error { return nil }, blockedSetup)
	if err == nil {
		t.Fatal("user cancellation swallowed as a time slice")
	}
	process := chromedp.FromContext(session.root).Browser.Process()
	profile := session.profile
	if err = process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = session.Healthy(context.Background()); err == nil {
		t.Fatal("dead process reported healthy")
	}
	session.Close()
	if _, err = os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("temporary profile leaked", err)
	}
}

func TestCollectionUsesBrowserLifetime(t *testing.T) {
	cfg := DefaultConfig()
	if got := collectionTimeout(cfg, TargetIdentity{}); got != 6*time.Hour {
		t.Fatal("default browser lifetime not used", got)
	}
	cfg.BrowserMaxAge = 24 * 3600
	if got := collectionTimeout(cfg, TargetIdentity{DurableProgress: true, Catchup: true, CatchupScrollBudget: 1000}); got != 24*time.Hour {
		t.Fatal(got)
	}
	cfg.BrowserMaxAge = 30
	if got := collectionTimeout(cfg, TargetIdentity{DurableProgress: true, Catchup: true, CatchupScrollBudget: 1000}); got != 30*time.Second {
		t.Fatal(got)
	}
	if got := collectionTimeout(cfg, TargetIdentity{Catchup: true, CatchupScrollBudget: 1000}); got != 30*time.Second {
		t.Fatal("non-durable catch-up ignored configured timeout", got)
	}
}

func TestCollectionLimitSeparatesCLIAndWebSafetyCeilings(t *testing.T) {
	if err := validateCollectionLimit(10000); err != nil {
		t.Fatal(err)
	}
	if err := validateCollectionLimit(CollectionSafetyMaxPosts); err != nil {
		t.Fatal("web safety ceiling rejected by collector", err)
	}
	for _, limit := range []int{0, -1, CollectionSafetyMaxPosts + 1} {
		if err := validateCollectionLimit(limit); err == nil {
			t.Fatalf("invalid collection limit accepted: %d", limit)
		}
	}
}

func TestHistoricalModesIgnoreKnownPostBoundary(t *testing.T) {
	for _, mode := range []string{"backfill", "rescan"} {
		if usesIncrementalBoundary(TargetIdentity{CollectionMode: mode}) {
			t.Fatalf("%s unexpectedly uses the incremental known-post boundary", mode)
		}
	}
	for _, mode := range []string{"", "incremental"} {
		if !usesIncrementalBoundary(TargetIdentity{CollectionMode: mode}) {
			t.Fatalf("%q unexpectedly disabled the incremental known-post boundary", mode)
		}
	}
}

func TestTimelineFailureRetainsDiagnosticCategory(t *testing.T) {
	for _, tc := range []struct {
		status       int64
		reason, want string
	}{
		{503, "", "HTTP 503"}, {200, "浏览器响应读取超时", "浏览器响应读取超时"}, {0, "浏览器网络请求失败", "浏览器网络请求失败"}, {200, "响应解析失败或接口返回临时错误", "响应解析失败或接口返回临时错误"},
	} {
		meta := responseMeta{operation: "UserOriginalsTimeline", status: tc.status}
		var err error
		if tc.reason == "" {
			err = timelineFailure(meta)
		} else {
			err = timelineFailure(meta, tc.reason)
		}
		if FailureKind(err) != "timeline" || !strings.Contains(err.Error(), tc.want) {
			t.Fatal(err)
		}
	}
}
