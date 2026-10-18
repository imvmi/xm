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
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			cfg := Config{DataDir: dir, Headless: true, CollectionTimeout: 20, ScrollDelay: 1, MaxScrolls: 3}
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
							body = `<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>(async()=>{await fetch('/i/api/graphql/fixture/UserByScreenName');await new Promise(r=>setTimeout(r,300));await fetch('/i/api/graphql/fixture/` + test.operation + `');})()</script></body></html>`
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
			result, err := collect(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", 2, test.operation == "UserRepliesTimeline", func(p Page) error {
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

func TestBrowserSessionCollectsMultipleTargetsOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("set XM_BROWSER_TEST=1 to run real-Chrome fixture integration")
	}
	cfg := Config{Headless: true, CollectionTimeout: 20, ScrollDelay: 1, MaxScrolls: 3}
	session, err := NewBrowserSession(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
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
		result, collectErr := session.collectBound(context.Background(), username, TargetIdentity{}, 2, false, func(Page) error { return nil }, setup)
		if collectErr != nil || result.Posts != 2 || result.Stop != "limit_reached" {
			t.Fatalf("shared session failed for %s: %+v err=%v", username, result, collectErr)
		}
	}
}
