package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

func TestBrowserDeepCatchupOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("set XM_BROWSER_TEST=1 for real-Chrome fixture integration")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	posts := []Post{{ID: "1", AuthorID: "42", Username: "alice"}}
	entries := []any{}
	for i := 5100; i >= 97; i-- {
		id := fmt.Sprint(i)
		if i >= 100 {
			posts = append(posts, Post{ID: id, AuthorID: "42", Username: "alice"})
		}
		entries = append(entries, catchupFixtureEntry(id))
	}
	entries = append(entries, catchupFixtureEntry("1"))
	if err = s.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(object{"data": object{"user": object{"result": object{"timeline": object{"instructions": []any{object{"type": "TimelineAddEntries", "entries": entries}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	// A large synthetic response puts archived IDs outside the old 4,000-ID
	// window before the missing posts. Every request is intercepted locally.
	setup := func(ctx context.Context) error {
		if err := chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
			_, err := cdp.Call(c, target, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}})
			return err
		})); err != nil {
			return err
		}
		events := chromedp.Events(ctx, fetch.RequestPaused)
		go func() {
			for e, err := range events {
				if err != nil {
					return
				}
				data, ct := []byte{}, "text/html"
				switch {
				case strings.Contains(e.Request.URL, "/UserByScreenName"):
					data, ct = []byte(`{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"alice"}}}}}`), "application/json"
				case strings.Contains(e.Request.URL, "/UserTweets"):
					data, ct = body, "application/json"
				case strings.HasSuffix(e.Request.URL, "/alice"):
					data = []byte(`<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>
(async()=>{await fetch('/i/api/graphql/fixture/UserByScreenName');await fetch('/i/api/graphql/fixture/UserTweets');})();
</script></body></html>`)
				}
				_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
					_, err := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: 200, ResponseHeaders: []*fetch.HeaderEntry{{Name: "Content-Type", Value: ct}}, Body: data})
					return err
				}))
			}
		}()
		return nil
	}
	cfg := Config{Headless: true, BrowserMaxAge: 30, ScrollDelay: 1, MaxScrolls: 20}
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, CatchupPostAnchor: "1"}
	identity.LookupKnownPosts = func(ids []string) (map[string]bool, error) {
		return s.KnownTargetPostIDs(context.Background(), "alice", ids)
	}
	for round, stop := range []string{"limit_reached", "known_boundary_reached"} {
		result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 2, false, func(p Page) error {
			return s.SavePosts("alice", p.Posts)
		}, setup)
		if err != nil || result.Stop != stop {
			t.Fatalf("round %d: result=%+v err=%v", round, result, err)
		}
		known, err := s.KnownTargetPostIDs(context.Background(), "alice", []string{"99", "98", "97"})
		if err != nil || !known["99"] || !known["98"] || known["97"] != (round == 1) {
			t.Fatalf("round %d made no forward progress: %v, %v", round, known, err)
		}
	}
	lookupErr := errors.New("injected archive lookup failure")
	identity.LookupKnownPosts = func([]string) (map[string]bool, error) { return nil, lookupErr }
	saved := 0
	_, err = collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 2, false, func(p Page) error {
		saved += len(p.Posts)
		return nil
	}, setup)
	if !errors.Is(err, lookupErr) || saved != 0 {
		t.Fatal("lookup failure must stop before saving posts", saved, err)
	}
}

func catchupFixtureEntry(id string) object {
	return object{"entryId": "tweet-" + id, "content": object{"itemContent": object{"tweet_results": object{"result": object{
		"__typename": "Tweet", "rest_id": id,
		"core":   object{"user_results": object{"result": object{"rest_id": "42", "core": object{"screen_name": "alice"}}}},
		"legacy": object{"full_text": "synthetic catchup post"},
	}}}}}
}
