package archive

import (
	"context"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserSharedBudgetSavesCursorAndResumesOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	s, b, _ := testBudget(t)
	// Leave two requests: profile plus the first page. The second page must never
	// reach the fixture's upstream handler, even though it uses another endpoint.
	for i := 0; i < requestBudgetLimit-2; i++ {
		if err := b.Acquire(context.Background(), "UserTweets"); err != nil {
			t.Fatal(err)
		}
	}
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", Catchup: true, DurableProgress: true, RequestBudget: b}
	cfg := Config{Headless: true, CollectionTimeout: 30, ScrollDelay: 1, MaxScrolls: 20}
	f := &resumeFixture{pages: 3, failPage: -1, budget: b}
	result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, f.setup)
	if FailureKind(err) != "budget" || result.Posts != 2 {
		t.Fatalf("%+v %v", result, err)
	}
	if len(f.seen()) != 1 {
		t.Fatal("request reached upstream beyond budget", f.seen())
	}
	identity.Progress, err = s.LoadCollectionProgress("alice", "42")
	if err != nil || identity.Progress.Posts.Cursor != "page-1" {
		t.Fatal(identity.Progress, err)
	}
	again, err := s.LoadRequestBudget(Cookies{AuthToken: "synthetic-budget"})
	if err != nil {
		t.Fatal(err)
	}
	again.now = func() time.Time { return time.Now().Add(20 * time.Minute) }
	again.spacing = 0
	identity.RequestBudget = again
	next := &resumeFixture{pages: 3, failPage: -1, budget: again}
	result, err = collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(p Page) error { return s.SaveCollectionPage("alice", 0, p) }, next.setup)
	if err != nil || result.Posts != 4 {
		t.Fatalf("resume %+v %v", result, err)
	}
	if next.seen()[0] != "UserTweets:page-1" {
		t.Fatal(next.seen())
	}
}
func TestBrowserSuccessHeadersStopBeforeNextRequestOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	_, b, _ := testBudget(t)
	f := &resumeFixture{pages: 3, failPage: -1, budget: b, headers: []*fetch.HeaderEntry{{Name: "x-rate-limit-limit", Value: "50"}, {Name: "x-rate-limit-remaining", Value: "3"}}}
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", DurableProgress: true, RequestBudget: b}
	cfg := Config{Headless: true, CollectionTimeout: 30, ScrollDelay: 1, MaxScrolls: 20}
	result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(Page) error { return nil }, f.setup)
	if FailureKind(err) != "budget" || result.Posts != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if len(f.seen()) != 0 {
		t.Fatal("timeline sent despite exhausted header", f.seen())
	}
}

func TestBrowserBudgetInterceptorBlocksNetworkOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	_, b, _ := testBudget(t)
	for i := 0; i < requestBudgetLimit-2; i++ {
		if err := b.Acquire(context.Background(), "UserTweets"); err != nil {
			t.Fatal(err)
		}
	}
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/i/api/graphql/") {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(`<html><body>local fixture</body></html>`))
	}))
	defer server.Close()
	url := strings.Replace(server.URL, "127.0.0.1", "x.com", 1)
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.Flag("host-resolver-rules", "MAP x.com 127.0.0.1"), chromedp.Flag("ignore-certificate-errors", true), chromedp.Flag("no-proxy-server", true))
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alloc, closeAlloc := chromedp.NewExecAllocator(parent, opts...)
	defer closeAlloc()
	ctx, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	if err := chromedp.Do(ctx, chromedp.Navigate(url)); err != nil {
		t.Fatal(err)
	}
	failures := make(chan error, 1)
	if err := installRequestBudget(ctx, b, failures); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{200, 200, localBudgetStatus} {
		status, err := chromedp.Run(ctx, chromedp.Evaluate[int](`fetch('/i/api/graphql/fixture/UserTweets').then(r=>r.status)`, chromedp.EvalAwaitPromise))
		if err != nil || status != want {
			t.Fatal(status, want, err)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("blocked request reached server", requests.Load())
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}
func TestBrowserPacingDoesNotLookLikeTimelineEndOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	_, b, _ := testBudget(t)
	b.now = time.Now
	b.spacing = 9 * time.Second
	f := &resumeFixture{pages: 1, failPage: -1, budget: b}
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", DurableProgress: true, RequestBudget: b}
	cfg := Config{Headless: true, CollectionTimeout: 30, ScrollDelay: 1, MaxScrolls: 20}
	result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(Page) error { return nil }, f.setup)
	if err != nil || result.Posts != 2 || result.Stop != "known_boundary_reached" {
		t.Fatalf("%+v %v", result, err)
	}
}
