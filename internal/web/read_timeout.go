package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

const postReadTimeout = 15 * time.Second

// Keep the handler synchronous: canceled database operations must finish and
// release their connections before returning. Buffer JSON so a deadline during
// enrichment cannot turn a partial result into a successful response. Streaming
// media and mutations do not use this wrapper.
func readTimeout(timeout time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		b := newAPIBuffer()
		b.header = w.Header().Clone()
		if ctx.Err() == nil {
			next(b, r.WithContext(ctx))
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			if strings.HasPrefix(r.URL.Path, "/api/v1/") {
				apiError(w, http.StatusGatewayTimeout, "query_timeout", "查询超时，请缩小搜索范围后重试")
			} else {
				problem(w, http.StatusGatewayTimeout, "查询超时，请缩小搜索范围后重试")
			}
			return
		}
		if ctx.Err() != nil {
			// The client disconnected; do not publish stale results.
			w.WriteHeader(499)
			return
		}
		for key, values := range b.Header() {
			w.Header()[key] = values
		}
		status := b.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(b.body.Bytes())
	}
}
