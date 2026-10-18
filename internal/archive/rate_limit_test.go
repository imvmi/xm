package archive

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
)

func TestRateLimitRetryAt(t *testing.T) {
	now := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		headers network.Headers
		want    time.Time
	}{
		{"seconds", network.Headers{"Retry-After": "120"}, now.Add(2 * time.Minute)},
		{"numeric", network.Headers{"retry-after": float64(120)}, now.Add(2 * time.Minute)},
		{"date", network.Headers{"RETRY-AFTER": now.Add(time.Hour).Format(http.TimeFormat)}, now.Add(time.Hour)},
		{"reset", network.Headers{"X-Rate-Limit-Reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}, now.Add(time.Hour)},
		{"later_reset", network.Headers{"Retry-After": "120", "x-rate-limit-reset": now.Add(time.Hour).Unix()}, now.Add(time.Hour)},
		{"later_retry", network.Headers{"Retry-After": "7200", "x-rate-limit-reset": now.Add(time.Hour).Unix()}, now.Add(2 * time.Hour)},
		{"past", network.Headers{"Retry-After": now.Add(-time.Hour).Format(http.TimeFormat), "x-rate-limit-reset": now.Unix() - 1}, time.Time{}},
		{"malformed", network.Headers{"Retry-After": "nonsense", "x-rate-limit-reset": "123.45"}, time.Time{}},
		{"overflow", network.Headers{"Retry-After": "9223372036854775807", "x-rate-limit-reset": "9223372036854775807"}, time.Time{}},
		{"negative", network.Headers{"Retry-After": "-1", "x-rate-limit-reset": "-1"}, time.Time{}},
		{"missing", nil, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateLimitRetryAt(tc.headers, now); !got.Equal(tc.want) {
				t.Fatalf("retry time = %s, want %s", got, tc.want)
			}
		})
	}
}
