package archive

import (
	"context"
	"os"
	"strings"
	"testing"

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
	for _, test := range []struct {
		name, body string
		wantErr    bool
	}{
		{"authorized", string(fixture(t, "timeline.json")), false},
		{"protected_denied", `{"errors":[{"code":179}]}`, true},
		{"expired_cookie", `{"errors":[{"code":89}]}`, true},
		{"rate_limited", `{"errors":[{"code":88}]}`, true},
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
						switch {
						case strings.Contains(e.Request.URL, "/UserByScreenName"):
							ct = "application/json"
							body = `{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"alice"},"privacy":{"protected":true}}}}}`
						case strings.Contains(e.Request.URL, "/UserTweets"):
							ct = "application/json"
							body = test.body
						case strings.HasSuffix(e.Request.URL, "/alice"):
							body = `<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>(async()=>{await fetch('/i/api/graphql/fixture/UserByScreenName');await new Promise(r=>setTimeout(r,300));await fetch('/i/api/graphql/fixture/UserTweets');})()</script></body></html>`
						}
						_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
							_, err := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: 200, ResponseHeaders: []*fetch.HeaderEntry{{Name: "Content-Type", Value: ct}}, Body: []byte(body)})
							return err
						}))
					}
				}()
				return nil
			}
			result, err := collect(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", 2, false, func(p Page) error {
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
