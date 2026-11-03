package archive

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

func TestIdentityRefreshBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome identity fixture")
	}
	var timelineRate atomic.Bool
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
				body, ct := "", "text/html"
				switch {
				case strings.Contains(e.Request.URL, "/TweetDetail"):
					ct = "application/json"
					body = `{"data":{"items":[{"rest_id":"200","core":{"user_results":{"result":{"rest_id":"42","core":{"screen_name":"newalice"}}}}}]}}`
				case strings.Contains(e.Request.URL, "/UserByScreenName"):
					ct = "application/json"
					body = `{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"newalice"},"privacy":{"protected":true}}}}}`
				case strings.Contains(e.Request.URL, "/UserTweets"):
					ct = "application/json"
					body = `{"errors":[{"code":179}]}`
					if timelineRate.Load() {
						body = `{"errors":[{"code":88}]}`
					}
				case strings.Contains(e.Request.URL, "/i/status/"):
					id := e.Request.URL[strings.LastIndex(e.Request.URL, "/")+1:]
					body = `<button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>fetch('/i/api/graphql/fixture/TweetDetail?variables=%7B%22focalTweetId%22%3A%22` + id + `%22%7D')</script>`
				case strings.HasSuffix(e.Request.URL, "/newalice"):
					// Explicit request subjects detect accidentally retaining the old handle
					// after following an archived post; timeline errors must be ignored here.
					body = `<button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>(async()=>{await fetch('/i/api/graphql/fixture/UserTweets?variables=%7B%22userId%22%3A%2242%22%7D');await fetch('/i/api/graphql/fixture/UserByScreenName?variables=%7B%22screen_name%22%3A%22newalice%22%7D')})()</script>`
				}
				_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
					_, err := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: 200, ResponseHeaders: []*fetch.HeaderEntry{{Name: "Content-Type", Value: ct}}, Body: []byte(body)})
					return err
				}))
			}
		}()
		return nil
	}
	cfg := Config{Headless: true, BrowserMaxAge: 20, ScrollDelay: 1, MaxScrolls: 3}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var attempts []string
	p, err := resolveTargetProfile(ctx, TargetIdentity{UserID: "42", CurrentUsername: "oldalice"}, []string{"300", "200"}, "", func(ctx context.Context, i TargetIdentity) (Profile, error) {
		attempts = append(attempts, i.ReferencePostID)
		return lookupTargetProfile(ctx, cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, i, setup)
	})
	if err != nil || p.ID != "42" || p.Username != "newalice" || !p.Protected || len(attempts) != 2 {
		t.Fatal(p, attempts, err)
	}
	// An account using the supplied handle must still have the bound ID.
	_, err = lookupTargetProfile(ctx, cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, TargetIdentity{UserID: "99", CurrentUsername: "newalice"}, setup)
	if FailureKind(err) != "identity" {
		t.Fatal("wrong user accepted", err)
	}
	timelineRate.Store(true)
	_, err = lookupTargetProfile(ctx, cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, TargetIdentity{UserID: "42", CurrentUsername: "newalice"}, setup)
	if FailureKind(err) != "rate" {
		t.Fatal("profile lookup ignored timeline rate limit", err)
	}

}
