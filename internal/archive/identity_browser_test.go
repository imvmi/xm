package archive

import (
	"context"
	"fmt"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
	"os"
	"strings"
	"testing"
)

// All requests are fulfilled locally before networking, including takeovers,
// protected accounts and replies. Production credentials are never used.
func TestBrowserIdentityOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("set XM_BROWSER_TEST=1")
	}
	for _, tc := range []struct {
		name                                      string
		wrong, missing, replies, takeover, modern bool
	}{
		{name: "rename_and_out_of_order"}, {name: "renamed_replies", replies: true}, {name: "renamed_modern_replies", replies: true, modern: true}, {name: "wrong_resolved_identity", wrong: true}, {name: "deleted_reference", missing: true}, {name: "old_handle_taken_over", takeover: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Headless: true, CollectionTimeout: 30, ScrollDelay: 1, MaxScrolls: 3}
			profileID := "42"
			if tc.wrong {
				profileID = "99"
			}
			operation := "UserTweets"
			if tc.replies {
				operation = "UserTweetsAndReplies"
				if tc.modern {
					operation = "UserRepliesTimeline"
				}
			}
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
						case strings.Contains(e.Request.URL, "/TweetDetail"):
							ct = "application/json"
							body = `{"data":{"rest_id":"123","core":{"user_results":{"result":{"rest_id":"42","core":{"screen_name":"newalice"}}}}}}`
							if tc.missing {
								body = `{"data":{}}`
							}
						case strings.Contains(e.Request.URL, "/UserByScreenName"):
							ct = "application/json"
							id, name := profileID, "newalice"
							if strings.Contains(e.Request.URL, "?old") {
								id, name = "99", "alice"
							}
							body = fmt.Sprintf(`{"data":{"user":{"result":{"rest_id":%q,"core":{"screen_name":%q},"privacy":{"protected":true}}}}}`, id, name)
						case strings.Contains(e.Request.URL, "/"+operation):
							ct = "application/json"
							body = string(fixture(t, "timeline.json")) // old handle, same author ID
						case strings.HasSuffix(e.Request.URL, "/alice"):
							body = `<button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>fetch('/i/api/graphql/f/UserByScreenName?old')</script>`
						case strings.Contains(e.Request.URL, "/i/status/123"):
							body = `<button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>fetch('/i/api/graphql/f/TweetDetail')</script>`
						case strings.HasSuffix(e.Request.URL, "/newalice") || strings.HasSuffix(e.Request.URL, "/newalice/with_replies"):
							body = `<button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>(async()=>{await fetch('/i/api/graphql/f/` + operation + `');await new Promise(r=>setTimeout(r,100));await fetch('/i/api/graphql/f/UserByScreenName');})()</script>`
						}
						_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
							_, err := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: 200, ResponseHeaders: []*fetch.HeaderEntry{{Name: "Content-Type", Value: ct}}, Body: []byte(body)})
							return err
						}))
					}
				}()
				return nil
			}
			identity := TargetIdentity{UserID: "42", ReferencePostID: "123"}
			if tc.takeover {
				identity.CurrentUsername = "alice"
			}
			saved, verified := 0, false
			result, err := collectIdentity(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 2, tc.replies, func(p Page) error {
				if p.Profile != nil {
					if p.Profile.ID != "42" || p.Profile.Username != "newalice" {
						t.Error("unverified profile saved")
					}
					verified = true
				}
				for _, post := range p.Posts {
					if !verified || post.AuthorID != "42" {
						t.Error("unverified post saved")
					}
					saved++
				}
				return nil
			}, setup)
			if tc.wrong || tc.missing {
				if FailureKind(err) != "identity" || saved != 0 {
					t.Fatal("unsafe failure", result, err, saved)
				}
				return
			}
			if err != nil || saved != 2 || !verified {
				t.Fatal(result, err, saved, verified)
			}
		})
	}
}
