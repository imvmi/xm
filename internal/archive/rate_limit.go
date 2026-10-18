package archive

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
)

// Capture the deadline at receipt, before body reads or media downloads can
// delay processing. Header names are case insensitive; the later hint wins.
func rateLimitRetryAt(headers network.Headers, now time.Time) time.Time {
	var retryAt time.Time
	for name, raw := range headers {
		value := strings.TrimSpace(fmt.Sprint(raw))
		var candidate time.Time
		switch strings.ToLower(name) {
		case "retry-after":
			if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
				// Avoid overflowing time.Duration with a malformed upstream value.
				if seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
					candidate = now.Add(time.Duration(seconds) * time.Second)
				}
			} else if date, err := http.ParseTime(value); err == nil {
				candidate = date
			}
		case "x-rate-limit-reset":
			if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= 253402300799 {
				candidate = time.Unix(seconds, 0)
			}
		}
		if candidate.After(now) && candidate.After(retryAt) {
			retryAt = candidate
		}
	}
	return retryAt
}

func rateLimitFailure(meta responseMeta) error {
	return &Failure{
		Kind: "rate", Message: fmt.Sprintf("X %s 请求限流，请等待后重试", meta.operation),
		RetryAt: meta.retryAt,
	}
}
