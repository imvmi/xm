package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

// A media-heavy first page may be taller than dozens of viewport scrolls.
// Intercept every request; no real X credentials or network access are used.
func TestBrowserTallTimelinePaginationOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("set XM_BROWSER_TEST=1 for real-Chrome fixture integration")
	}
	page := func(start int) string {
		entries := []any{}
		for i := start; i < start+50; i++ {
			id := fmt.Sprint(1000 + i)
			entries = append(entries, object{"entryId": "tweet-" + id, "content": object{"itemContent": object{"tweet_results": object{"result": object{
				"__typename": "Tweet", "rest_id": id,
				"core":   object{"user_results": object{"result": object{"rest_id": "42", "core": object{"screen_name": "alice"}}}},
				"legacy": object{"full_text": "synthetic pagination post"},
			}}}}})
		}
		b, err := json.Marshal(object{"data": object{"user": object{"result": object{"timeline": object{"instructions": []any{object{"type": "TimelineAddEntries", "entries": entries}}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first, second := page(0), page(50)
	for _, tc := range []struct {
		name, op, path, stop string
		replies, known       bool
		limit, count, pages  int
	}{
		{"posts_reach_limit", "UserTweets", "/alice", "limit_reached", false, false, 60, 60, 2},
		{"posts_exhaustion", "UserTweets", "/alice", "no_new_posts_observed", false, false, 120, 100, 2},
		{"incremental_boundary", "UserTweets", "/alice", "known_boundary_reached", false, true, 120, 50, 1},
		{"incremental_lookup_boundary", "UserTweets", "/alice", "known_boundary_reached", false, true, 120, 50, 1},
		{"replies_then_originals", "UserRepliesTimeline", "/alice/with_replies", "limit_reached", true, false, 80, 80, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := func(ctx context.Context) error {
				if err := chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
					_, e := cdp.Call(c, target, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}})
					return e
				})); err != nil {
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
							body, ct = `{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"alice"}}}}}`, "application/json"
						case strings.Contains(e.Request.URL, "/UserTweets") || strings.Contains(e.Request.URL, "/UserRepliesTimeline"):
							body, ct = first, "application/json"
							if strings.Contains(e.Request.URL, "page=2") && !strings.Contains(e.Request.URL, "/UserRepliesTimeline") {
								body = second
							}
						case strings.HasSuffix(e.Request.URL, tc.path) || strings.HasSuffix(e.Request.URL, "/alice"):
							op := tc.op
							if strings.HasSuffix(e.Request.URL, "/alice") {
								op = "UserTweets"
							}
							body = `<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><div style="height:100000px">Tall media timeline</div><script>
(async()=>{await fetch('/i/api/graphql/fixture/UserByScreenName');await fetch('/i/api/graphql/fixture/` + op + `');
let loaded=false;addEventListener('scroll',async()=>{if(loaded||scrollY+innerHeight<document.documentElement.scrollHeight-32)return;loaded=true;await new Promise(r=>setTimeout(r,400));await fetch('/i/api/graphql/fixture/` + op + `?page=2');});})();
</script></body></html>`
						}
						_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
							_, e := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: 200, ResponseHeaders: []*fetch.HeaderEntry{{Name: "Content-Type", Value: ct}}, Body: []byte(body)})
							return e
						}))
					}
				}()
				return nil
			}
			cfg := Config{Headless: true, BrowserMaxAge: 30, ScrollDelay: 1, MaxScrolls: 20, TimelineIdleSeconds: 3}
			stored := map[string]bool{}
			identity := TargetIdentity{}
			if tc.known {
				identity = TargetIdentity{UserID: "42", CurrentUsername: "alice", KnownPostIDs: map[string]bool{}}
				for i := range 50 {
					identity.KnownPostIDs[fmt.Sprint(1000+i)] = true
				}
				if tc.name == "incremental_lookup_boundary" {
					known := identity.KnownPostIDs
					identity.KnownPostIDs = nil
					identity.LookupKnownPosts = func(ids []string) (map[string]bool, error) {
						found := map[string]bool{}
						for _, id := range ids {
							if known[id] {
								found[id] = true
							}
						}
						return found, nil
					}
				}
			}
			result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, tc.limit, tc.replies, func(p Page) error {
				for _, post := range p.Posts {
					stored[post.ID] = true
				}
				return nil
			}, setup)
			if err != nil || result.Posts != tc.count || len(stored) != tc.count || result.Pages != tc.pages || result.Stop != tc.stop {
				t.Fatalf("pagination failed: %+v stored=%d err=%v", result, len(stored), err)
			}
		})
	}
}
