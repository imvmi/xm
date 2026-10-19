package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func resumeFixtureBody(page, total int) []byte {
	entries := []any{catchupFixtureEntry(fmt.Sprint(100 - page*2)), catchupFixtureEntry(fmt.Sprint(99 - page*2))}
	var next any
	instructions := []any{}
	if page+1 < total {
		next = fmt.Sprintf("page-%d", page+1)
		entries = append(entries, object{"entryId": "cursor-bottom", "content": object{"cursorType": "Bottom", "value": next}})
	} else {
		instructions = append(instructions, object{"type": "TimelineTerminateTimeline", "direction": "Bottom"})
	}
	instructions = append(instructions, object{"type": "TimelineAddEntries", "entries": entries})
	body, _ := json.Marshal(object{"next": next, "data": object{"user": object{"result": object{"timeline": object{"instructions": instructions}}}}})
	return body
}

type resumeFixture struct {
	mu            sync.Mutex
	requests      []string
	pages         int
	failPage      int
	failOperation string
	networkFail   bool
	ignoreCursor  bool
	xhr           bool
	buffered      bool
	budget        *RequestBudget
	headers       []*fetch.HeaderEntry
}

func (f *resumeFixture) setup(ctx context.Context) error {
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
			if f.budget != nil {
				allowed, err := admitBudgetRequest(ctx, f.budget, &e)
				if err != nil {
					return
				}
				if !allowed {
					continue
				}
			}
			body, ct, status := []byte{}, "text/html", int64(200)
			u, _ := url.Parse(e.Request.URL)
			op := operation(e.Request.URL)
			switch {
			case op == "UserByScreenName":
				body, ct = []byte(`{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"alice"}}}}}`), "application/json"
			case op == "UserTweets" || op == "UserRepliesTimeline":
				cursor := requestCursor(e.Request.URL)
				page := 0
				if cursor != "" {
					_, _ = fmt.Sscanf(cursor, "page-%d", &page)
				}
				f.mu.Lock()
				f.requests = append(f.requests, op+":"+cursor)
				f.mu.Unlock()
				if page == f.failPage && f.networkFail && (f.failOperation == "" || op == f.failOperation) {
					_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
						_, err := cdp.Call(c, target, fetch.FailRequest, fetch.FailRequestParams{RequestID: e.RequestID, ErrorReason: network.ErrorReasonFailed})
						return err
					}))
					continue
				}
				ct = "application/json"
				if page == f.failPage && (f.failOperation == "" || op == f.failOperation) {
					body, status = []byte(`{"errors":[{"code":88}]}`), 429
				} else {
					body = resumeFixtureBody(page, f.pages)
				}
			case u.Path == "/alice" || u.Path == "/alice/with_replies":
				op := "UserTweets"
				if strings.HasSuffix(u.Path, "/with_replies") {
					op = "UserRepliesTimeline"
				}
				variables := `{userId:'42'}`
				if f.ignoreCursor {
					variables = `{userId:'wrong'}`
				}
				transport := `fetch(url).then(r=>r.json())`
				if f.xhr {
					transport = `new Promise(resolve=>{const x=new XMLHttpRequest();x.open('GET',url);x.onload=()=>resolve(JSON.parse(x.responseText));x.send();})`
				}
				body = []byte(`<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><div id="tall" style="height:2000px"></div><script>
let next,busy=false;async function load(cursor){busy=true;const v=` + variables + `;if(cursor)v.cursor=cursor;
const url='/i/api/graphql/fixture/` + op + `?variables='+encodeURIComponent(JSON.stringify(v));const b=await ` + transport + `;next=b.next;document.getElementById('tall').style.height=(parseInt(document.getElementById('tall').style.height)+1000)+'px';busy=false;}
(async()=>{await fetch('/i/api/graphql/fixture/UserByScreenName');await load();})();
addEventListener('scroll',()=>{if(!busy&&next&&scrollY+innerHeight>=document.documentElement.scrollHeight-32)load(next);});
</script></body></html>`)
				if f.buffered {
					body = []byte(`<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><script>
(async()=>{for(let i=0;i<` + fmt.Sprint(f.pages) + `;i++){const v={userId:'42'};if(i)v.cursor='page-'+i;await fetch('/i/api/graphql/fixture/UserTweets?variables='+encodeURIComponent(JSON.stringify(v)));}await fetch('/i/api/graphql/fixture/UserByScreenName');})();
</script></body></html>`)
				}
			}
			_ = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
				_, err := cdp.Call(c, target, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: status, ResponseHeaders: append([]*fetch.HeaderEntry{{Name: "Content-Type", Value: ct}}, f.headers...), Body: body})
				return err
			}))
		}
	}()
	return nil
}

func (f *resumeFixture) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.requests...)
}

func TestBrowserResumeAfterRateLimitOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	for _, xhr := range []bool{false, true} {
		t.Run(fmt.Sprintf("xhr_%t", xhr), func(t *testing.T) {
			s, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true}
			identity.LookupKnownPosts = func(ids []string) (map[string]bool, error) {
				return s.KnownTargetPostIDs(context.Background(), "alice", ids)
			}
			cfg := Config{Headless: true, BrowserMaxAge: 30, ScrollDelay: 1, MaxScrolls: 20}
			first := &resumeFixture{pages: 3, failPage: 1, xhr: xhr}
			result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, first.setup)
			if FailureKind(err) != "rate" || result.Posts != 2 {
				t.Fatalf("first run: %+v %v", result, err)
			}
			identity.Progress, err = s.LoadCollectionProgress("alice", "42")
			if err != nil {
				t.Fatal(err)
			}
			if identity.Progress.Posts.Cursor != "page-1" || identity.Progress.Posts.Head != "100" {
				t.Fatal(identity.Progress)
			}
			second := &resumeFixture{pages: 3, failPage: -1, xhr: xhr}
			result, err = collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, second.setup)
			if err != nil || result.Posts != 4 || !result.NeedsHeadRefresh || result.RefreshPostAnchor != "100" {
				t.Fatalf("resume: %+v %v", result, err)
			}
			if got := strings.Join(second.seen(), ","); got != "UserTweets:page-1,UserTweets:page-2" {
				t.Fatal("replayed old pages", got)
			}
		})
	}
}

func TestBrowserResumeAfterTimelineNetworkFailureOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true}
	identity.LookupKnownPosts = func(ids []string) (map[string]bool, error) {
		return s.KnownTargetPostIDs(context.Background(), "alice", ids)
	}
	cfg := Config{Headless: true, BrowserMaxAge: 30, ScrollDelay: 1, TimelineIdleSeconds: 3, MaxScrolls: 20}
	first := &resumeFixture{pages: 3, failPage: 1, networkFail: true}
	result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error {
		return s.SaveCollectionPage("alice", 0, p)
	}, first.setup)
	if FailureKind(err) != "timeline" || result.Posts != 2 {
		t.Fatalf("network failure was not retryable: %+v %v", result, err)
	}
	identity.Progress, err = s.LoadCollectionProgress("alice", "42")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Progress.Posts.Cursor != "page-1" {
		t.Fatal("last successful cursor was not preserved", identity.Progress)
	}
	second := &resumeFixture{pages: 3, failPage: -1}
	result, err = collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error {
		return s.SaveCollectionPage("alice", 0, p)
	}, second.setup)
	if err != nil || result.Posts != 4 {
		t.Fatalf("resume after network failure: %+v %v", result, err)
	}
	if got := strings.Join(second.seen(), ","); got != "UserTweets:page-1,UserTweets:page-2" {
		t.Fatal("resume did not start at the saved cursor", got)
	}
}

func TestBrowserRunsUntilTimelineEndWithoutPageBudgetOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true}
	cfg := Config{Headless: true, BrowserMaxAge: 30, ScrollDelay: 1, MaxScrolls: 100}
	fixture := &resumeFixture{pages: 17, failPage: -1}
	result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, fixture.setup)
	if err != nil || result.Stop != "known_boundary_reached" || result.Pages != 17 || result.Posts != 34 || len(fixture.seen()) != 17 {
		t.Fatalf("unbounded pages: %+v %v %v", result, err, fixture.seen())
	}
}

func TestBrowserResumeSkipsCompletedRepliesOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true}
	cfg := Config{Headless: true, BrowserMaxAge: 30, ScrollDelay: 1, MaxScrolls: 20}
	first := &resumeFixture{pages: 1, failPage: 0, failOperation: "UserTweets"}
	_, err = collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, true, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, first.setup)
	if FailureKind(err) != "rate" {
		t.Fatal(err)
	}
	identity.Progress, err = s.LoadCollectionProgress("alice", "42")
	if err != nil || !identity.Progress.Replies.Done {
		t.Fatal(identity.Progress, err)
	}
	second := &resumeFixture{pages: 1, failPage: -1}
	_, err = collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, true, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, second.setup)
	if err != nil || strings.Join(second.seen(), ",") != "UserTweets:" {
		t.Fatal("re-read completed replies", second.seen(), err)
	}
}

func TestBrowserUnappliedCheckpointStopsOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true, Progress: CollectionProgress{UserID: "42", Posts: TimelineProgress{Cursor: "page-1"}}}
	fixture := &resumeFixture{pages: 3, failPage: -1, ignoreCursor: true}
	saved := 0
	_, err := collectBound(context.Background(), Config{Headless: true, BrowserMaxAge: 20, ScrollDelay: 1, MaxScrolls: 20, TimelineIdleSeconds: 3}, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error { saved += len(p.Posts); return nil }, fixture.setup)
	if FailureKind(err) != "checkpoint" || saved != 0 || len(fixture.seen()) != 1 {
		t.Fatal(err, saved, fixture.seen())
	}
}

func TestBrowserPartialPageDoesNotSkipPostsOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true}
	identity.LookupKnownPosts = func(ids []string) (map[string]bool, error) {
		return s.KnownTargetPostIDs(context.Background(), "alice", ids)
	}
	cfg := Config{Headless: true, BrowserMaxAge: 20, ScrollDelay: 1, MaxScrolls: 20}
	for round, wantCursor := range []string{"", "page-1"} {
		fixture := &resumeFixture{pages: 2, failPage: -1}
		result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 1, false, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, fixture.setup)
		if err != nil || result.Stop != "limit_reached" {
			t.Fatal(round, result, err)
		}
		identity.Progress, err = s.LoadCollectionProgress("alice", "42")
		if err != nil || identity.Progress.Posts.Cursor != wantCursor {
			t.Fatal("advanced over unread part of page", round, identity.Progress, err)
		}
	}
	known, err := s.KnownTargetPostIDs(context.Background(), "alice", []string{"100", "99"})
	if err != nil || len(known) != 2 {
		t.Fatal("lost an item from partial page", known, err)
	}
}

func TestBrowserHeadRefreshFindsNewPostsOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	// Historical pages were already saved, but ID 100 arrived while resuming
	// them. Catch-up must return to the recorded head (98), not stop just because
	// other IDs encountered in between are already present in the archive.
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true, CatchupPostAnchor: "98", KnownPostIDs: map[string]bool{"99": true, "98": true, "97": true}}
	fixture := &resumeFixture{pages: 4, failPage: -1}
	saved := map[string]bool{}
	result, err := collectBound(context.Background(), Config{Headless: true, BrowserMaxAge: 20, ScrollDelay: 1, MaxScrolls: 20}, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error {
		for _, post := range p.Posts {
			saved[post.ID] = true
		}
		return nil
	}, fixture.setup)
	if err != nil || result.Stop != "known_boundary_reached" || !saved["100"] || result.NeedsHeadRefresh || len(fixture.seen()) != 2 {
		t.Fatal(result, saved, fixture.seen(), err)
	}
}

func TestBrowserBufferedPagesRunToCompletionOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fixture := &resumeFixture{pages: 18, failPage: -1, buffered: true}
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true}
	result, err := collectBound(context.Background(), Config{Headless: true, BrowserMaxAge: 40, ScrollDelay: 1, MaxScrolls: 20}, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, fixture.setup)
	if err != nil || result.Stop != "known_boundary_reached" || result.Pages != 18 || result.Posts != 36 {
		t.Fatal(result, err)
	}
	progress, err := s.LoadCollectionProgress("alice", "42")
	if err != nil || !progress.Posts.Exhausted {
		t.Fatal("completed buffered pages were not checkpointed", progress, err)
	}
}
