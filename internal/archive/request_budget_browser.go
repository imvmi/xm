package archive

import (
	"context"
	"errors"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

func ignoreCanceledInterception(err error) error {
	var protocolErr *cdproto.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != -32602 {
		return err
	}
	if protocolErr.Message != "Invalid InterceptionId" && protocolErr.Message != "Invalid InterceptionId." {
		return err
	}
	return nil
}

func admitBudgetRequest(ctx context.Context, b *RequestBudget, e *fetch.EventRequestPaused) (bool, error) {
	op := operation(e.Request.URL)
	if op == "" {
		return true, nil
	}
	if err := b.AcquireBrowser(ctx, op); err != nil {
		return false, err
	}
	return true, nil
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
					return ignoreCanceledInterception(err)
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
