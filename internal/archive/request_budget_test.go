package archive

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
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

func TestRequestBudgetConfiguredSpacingSurvivesRestart(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cookies := Cookies{AuthToken: "paced-requests"}
	b, err := s.LoadRequestBudget(cookies)
	if err != nil {
		t.Fatal(err)
	}
	if b.spacing != 5*time.Second || b.jitter != 0 {
		t.Fatal("wrong defaults", b.spacing, b.jitter)
	}
	now := time.Now()
	b.now = func() time.Time { return now }
	if delay, err := b.reserve("UserTweets"); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
	if delay, err := b.reserve("UserByScreenName"); err != nil || delay != 5*time.Second {
		t.Fatal(delay, err)
	}
	if b.state.LastRequest == 0 {
		t.Fatal("request time was not persisted")
	}
	cfg := DefaultConfig()
	cfg.RequestInterval = 12
	cfg.RequestJitter = 3
	if err := s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := s.LoadRequestBudget(cookies)
	if err != nil {
		t.Fatal(err)
	}
	again.now = b.now
	again.random = func(int64) int64 { return 0 }
	now = now.Add(4 * time.Second)
	if delay, err := again.reserve("UserTweets"); err != nil || delay != 5*time.Second {
		t.Fatal(delay, err)
	}
	now = now.Add(5 * time.Second)
	if delay, err := again.reserve("UserTweets"); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
	again.spacing = time.Hour
	again.jitter = 0
	now = now.Add(20 * time.Minute)
	if delay, err := again.reserve("UserTweets"); err != nil || delay != 40*time.Minute {
		t.Fatal(delay, err)
	}
}

func TestRequestBudgetRandomizedSpacingBoundsAndResamples(t *testing.T) {
	_, b, now := testBudget(t)
	b.spacing = 10 * time.Second
	b.jitter = 3 * time.Second
	values := []int64{0, 6}
	b.random = func(n int64) int64 {
		if n != 7 || len(values) == 0 {
			t.Fatalf("unexpected random request: %d", n)
		}
		value := values[0]
		values = values[1:]
		return value
	}
	if delay, err := b.reserve("UserTweets"); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
	if delay, err := b.reserve("UserTweets"); err != nil || delay != 7*time.Second {
		t.Fatal("lower bound", delay, err)
	}
	if delay, err := b.reserve("UserTweets"); err != nil || delay != 7*time.Second {
		t.Fatal("resampled while waiting", delay, err)
	}
	*now = now.Add(7 * time.Second)
	if delay, err := b.reserve("UserTweets"); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
	if delay, err := b.reserve("UserTweets"); err != nil || delay != 13*time.Second {
		t.Fatal("upper bound", delay, err)
	}
}

func TestRequestBudgetConcurrentPacing(t *testing.T) {
	_, b, _ := testBudget(t)
	b.spacing = 5 * time.Second
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			delay, err := b.reserve("UserTweets")
			if err != nil {
				t.Error(err)
			}
			if delay == 0 {
				admitted.Add(1)
			} else if delay != 5*time.Second {
				t.Error(delay)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatal("burst bypassed interval", admitted.Load())
	}
}

func TestRequestBudgetBrowserRequestsSharePageLoadCohort(t *testing.T) {
	_, b, _ := testBudget(t)
	b.now = time.Now
	b.spacing = 300 * time.Millisecond
	if err := b.AcquireBrowser(context.Background(), "UserByScreenName"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	ready := make(chan struct{})
	for _, op := range []string{"UserTweets", "UserByRestId", "UserRepliesTimeline"} {
		wg.Add(1)
		go func(op string) {
			defer wg.Done()
			<-ready
			errs <- b.AcquireBrowser(context.Background(), op)
		}(op)
	}
	close(ready)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 550*time.Millisecond {
		t.Fatal("concurrent browser requests were serialized", elapsed)
	}
	start = time.Now()
	if err := b.AcquireBrowser(context.Background(), "UserOriginalsTimeline"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatal("timeline request did not join the open cohort", elapsed)
	}
}

func TestRequestBudgetHasNoRollingRequestLimit(t *testing.T) {
	s, b, now := testBudget(t)
	for i := 0; i < 500; i++ {
		if err := b.Acquire(context.Background(), "UserTweets"); err != nil {
			t.Fatal(i, err)
		}
		*now = now.Add(time.Millisecond)
	}
	if len(b.state.Requests) != 0 || b.state.PauseUntil != 0 {
		t.Fatal("legacy quota state was retained", b.state)
	}
	restarted, err := s.LoadRequestBudget(Cookies{AuthToken: "synthetic-budget", CSRF: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = b.now
	restarted.spacing = 0
	if err = restarted.Acquire(context.Background(), "UserTweets"); err != nil {
		t.Fatal("restart introduced a request limit", err)
	}
}

func TestRequestBudgetHeadersAreTelemetryOnly(t *testing.T) {
	_, b, now := testBudget(t)
	reset := now.Add(time.Hour).Unix()
	for _, status := range []int64{200, 429} {
		if err := b.Observe("UserTweets", status, network.Headers{"x-rate-limit-limit": "50", "x-rate-limit-remaining": "0", "x-rate-limit-reset": reset}); err != nil {
			t.Fatal(err)
		}
		if err := b.Acquire(context.Background(), "UserRepliesTimeline"); err != nil {
			t.Fatal("response telemetry blocked the next request", err)
		}
		*now = now.Add(time.Millisecond)
	}
	endpoint := b.state.Endpoints["UserTweets"]
	if endpoint.Remaining == nil || *endpoint.Remaining != 0 || endpoint.Reset != reset || endpoint.Responses != 2 {
		t.Fatal(endpoint)
	}
}

func TestRequestBudgetIgnoresStaleAndMalformedHeaders(t *testing.T) {
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
	_ = b.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": 0, "x-rate-limit-reset": reset - 300})
	if got := *b.state.Endpoints["UserTweets"].Remaining; got != 8 {
		t.Fatal("old window replaced new", got)
	}
	_ = b.Observe("UserTweets", 200, network.Headers{"x-rate-limit-remaining": "garbage", "x-rate-limit-reset": "-1"})
	if err := b.Acquire(context.Background(), "UserTweets"); err != nil {
		t.Fatal(err)
	}
}

func TestRequestBudgetStorageFailureStopsDispatch(t *testing.T) {
	_, b, _ := testBudget(t)
	b.persist = func(requestBudgetState) error { return errors.New("db unavailable") }
	if err := b.Acquire(context.Background(), "UserTweets"); FailureKind(err) != "budget_storage" {
		t.Fatal(err)
	}
}

func TestRequestBudgetPacingCanCancel(t *testing.T) {
	_, b, _ := testBudget(t)
	b.spacing = time.Hour
	if err := b.Acquire(context.Background(), "UserTweets"); err != nil {
		t.Fatal(err)
	}
	last := b.state.LastRequest
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Acquire(ctx, "UserTweets"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if b.state.LastRequest != last {
		t.Fatal("cancelled request changed the pacing state")
	}
}
