package archive

import (
	"context"
	"errors"
	"github.com/chromedp/cdproto/network"
	"sync"
	"testing"
	"time"
)

func testBudget(t *testing.T) (*Store, *RequestBudget, *time.Time) {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b, err := s.LoadRequestBudget(Cookies{AuthToken: "synthetic-budget"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	b.now = func() time.Time { return now }
	b.spacing = 0
	return s, b, &now
}
func TestRequestBudgetSharedRollingWindowPersists(t *testing.T) {
	s, b, now := testBudget(t)
	for i := 0; i < requestBudgetLimit; i++ {
		if err := b.Acquire(context.Background(), []string{"UserTweets", "UserRepliesTimeline"}[i%2]); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
	}
	if err := b.Acquire(context.Background(), "UserByScreenName"); FailureKind(err) != "budget" {
		t.Fatal(err)
	}
	restarted, err := s.LoadRequestBudget(Cookies{AuthToken: "synthetic-budget", CSRF: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = b.now
	restarted.spacing = 0
	if err = restarted.Acquire(context.Background(), "UserTweets"); FailureKind(err) != "budget" {
		t.Fatal("restart reset quota", err)
	}
	*now = now.Add(requestBudgetWindow)
	if err = restarted.Acquire(context.Background(), "UserTweets"); err != nil {
		t.Fatal(err)
	}
	if len(restarted.state.Requests) != 1 {
		t.Fatal(restarted.state.Requests)
	}
}
func TestRequestBudgetHeadersPauseBefore429(t *testing.T) {
	s, b, now := testBudget(t)
	reset := now.Add(20 * time.Minute).Unix()
	if err := b.Observe("UserTweets", 200, network.Headers{"X-Rate-Limit-Limit": "50", "X-Rate-Limit-Remaining": "5", "X-Rate-Limit-Reset": reset}); err != nil {
		t.Fatal(err)
	}
	if !b.Until().Equal(time.Unix(reset+5, 0)) {
		t.Fatal(b.Until())
	}
	if err := b.Acquire(context.Background(), "UserRepliesTimeline"); FailureKind(err) != "budget" {
		t.Fatal(err)
	}
	again, err := s.LoadRequestBudget(Cookies{AuthToken: "synthetic-budget"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Until().Equal(b.Until()) {
		t.Fatal("header state lost")
	}
	*now = time.Unix(reset+6, 0)
	if err = b.Acquire(context.Background(), "UserTweets"); err != nil {
		t.Fatal(err)
	}
}
func TestRequestBudgetOutOfOrderMissingAndMalformedHeaders(t *testing.T) {
	_, b, now := testBudget(t)
	reset := now.Add(time.Hour).Unix()
	for _, remaining := range []int{8, 20} {
		if err := b.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": remaining, "x-rate-limit-reset": reset}); err != nil {
			t.Fatal(err)
		}
	}
	if got := *b.state.Endpoints["UserTweets"].Remaining; got != 8 {
		t.Fatal(got)
	}
	b.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": 0, "x-rate-limit-reset": reset - 300})
	if got := *b.state.Endpoints["UserTweets"].Remaining; got != 8 {
		t.Fatal("old window replaced new", got)
	}
	b.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": "garbage", "x-rate-limit-reset": "-1"})
	if b.Until().After(*now) {
		t.Fatal("malformed header blocked requests")
	}
	b.Observe("UserRepliesTimeline", 200, network.Headers{"x-rate-limit-remaining": "0"})
	if !b.Until().After(*now) {
		t.Fatal("missing reset ignored exhausted quota")
	}
}
func TestRequestBudgetConcurrentReservationsAndStorageFailure(t *testing.T) {
	_, b, _ := testBudget(t)
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = b.Acquire(context.Background(), "UserTweets") }()
	}
	wg.Wait()
	if len(b.state.Requests) != requestBudgetLimit {
		t.Fatal(len(b.state.Requests))
	}
	_, broken, _ := testBudget(t)
	broken.persist = func(requestBudgetState) error { return errors.New("db unavailable") }
	if err := broken.Acquire(context.Background(), "UserTweets"); FailureKind(err) != "budget_storage" {
		t.Fatal(err)
	}
}
func TestRequestBudgetPacingCanCancel(t *testing.T) {
	_, b, _ := testBudget(t)
	b.spacing = time.Hour
	if err := b.Acquire(context.Background(), "UserTweets"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Acquire(ctx, "UserTweets"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(b.state.Requests) != 1 {
		t.Fatal("cancelled request consumed quota")
	}
}
func TestRequestBudget429WithoutHeaders(t *testing.T) {
	_, b, now := testBudget(t)
	if err := b.Observe("UserTweets", 429, nil); err != nil {
		t.Fatal(err)
	}
	if b.Until().Before(now.Add(requestBudgetWindow)) {
		t.Fatal(b.Until())
	}
}
