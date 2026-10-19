package archive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

func TestIgnoreCanceledInterception(t *testing.T) {
	for _, message := range []string{"Invalid InterceptionId", "Invalid InterceptionId."} {
		err := fmt.Errorf("continue request: %w", &cdproto.Error{Code: -32602, Message: message})
		if ignored := ignoreCanceledInterception(err); ignored != nil {
			t.Fatalf("did not ignore canceled request %q: %v", message, ignored)
		}
	}
	for _, err := range []error{
		&cdproto.Error{Code: -32602, Message: "Invalid parameters"},
		&cdproto.Error{Code: -32000, Message: "Invalid InterceptionId."},
		errors.New("Invalid InterceptionId. (-32602)"),
	} {
		if got := ignoreCanceledInterception(err); got != err {
			t.Fatalf("ignored unrelated protocol error: %v", err)
		}
	}
}

func TestBrowserLowRemainingHeadersDoNotStopCollectionOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	_, b, now := testBudget(t)
	f := &resumeFixture{pages: 3, failPage: -1, budget: b, headers: []*fetch.HeaderEntry{{Name: "x-rate-limit-limit", Value: "50"}, {Name: "x-rate-limit-remaining", Value: "0"}, {Name: "x-rate-limit-reset", Value: fmt.Sprint(now.Add(time.Hour).Unix())}}}
	identity := TargetIdentity{UserID: "42", CurrentUsername: "alice", DurableProgress: true, RequestBudget: b}
	cfg := Config{Headless: true, CollectionTimeout: 30, ScrollDelay: 1, MaxScrolls: 20}
	result, err := collectBound(context.Background(), cfg, Cookies{AuthToken: "synthetic", CSRF: "synthetic"}, "alice", identity, 100, false, func(Page) error { return nil }, f.setup)
	if err != nil || result.Posts != 6 {
		t.Fatalf("%+v %v", result, err)
	}
	if len(f.seen()) != 3 {
		t.Fatal("low remaining header stopped pagination", f.seen())
	}
}

func TestBrowserPacerDoesNotImposeRollingLimitOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome fixtures")
	}
	_, b, _ := testBudget(t)
	b.now = time.Now
	b.spacing = 0
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/i/api/graphql/") {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`<html><body>local fixture</body></html>`))
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
	for i := 0; i < 120; i++ {
		status, err := chromedp.Run(ctx, chromedp.Evaluate[int](`fetch('/i/api/graphql/fixture/UserTweets').then(r=>r.status)`, chromedp.EvalAwaitPromise))
		if err != nil || status != 200 {
			t.Fatal(i, status, err)
		}
	}
	if requests.Load() != 120 {
		t.Fatal("request was locally blocked", requests.Load())
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
