package archive

import (
	"context"
	"errors"
	"strconv"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

// A local response travels through the same ordered response/body pipeline as
// real pages. The collector saves preceding pages before stopping for budget.
const localBudgetStatus = 470

func admitBudgetRequest(ctx context.Context, b *RequestBudget, e *fetch.EventRequestPaused) (bool, error) {
	op := operation(e.Request.URL)
	if op == "" {
		return true, nil
	}
	err := b.Acquire(ctx, op)
	if err == nil {
		return true, nil
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != "budget" {
		return false, err
	}
	err = chromedp.Do(ctx, chromedp.Func(func(c context.Context, t *chromedp.Target) error {
		_, err := cdp.Call(c, t, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: e.RequestID, ResponseCode: localBudgetStatus, ResponsePhrase: "Local Request Budget", ResponseHeaders: []*fetch.HeaderEntry{{Name: "Content-Type", Value: "application/json"}, {Name: "x-xm-budget-until", Value: strconv.FormatInt(failure.RetryAt.Unix(), 10)}}, Body: []byte(`{}`)})
		return err
	}))
	return false, err
}
func installRequestBudget(ctx context.Context, b *RequestBudget, failures chan<- error) error {
	events := chromedp.Events(ctx, fetch.RequestPaused)
	go func() {
		report := func(err error) {
			if err != nil && ctx.Err() == nil {
				select {
				case failures <- err:
				default:
				}
			}
		}
		for e, err := range events {
			if err != nil {
				report(err)
				return
			}
			// A slow response or paced request must not block other protocol events.
			go func(e *fetch.EventRequestPaused) {
				allowed, err := admitBudgetRequest(ctx, b, e)
				if err != nil {
					report(err)
					return
				}
				if !allowed {
					return
				}
				err = chromedp.Do(ctx, chromedp.Func(func(c context.Context, t *chromedp.Target) error {
					_, err := cdp.Call(c, t, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: e.RequestID})
					return err
				}))
				report(err)
			}(&e)
		}
	}()
	return chromedp.Do(ctx, chromedp.Func(func(c context.Context, t *chromedp.Target) error {
		_, err := cdp.Call(c, t, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "https://x.com/i/api/graphql/*"}, {URLPattern: "https://x.com:*/i/api/graphql/*"}}})
		return err
	}))
}
