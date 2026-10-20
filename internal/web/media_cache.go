package web

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

const (
	imageCacheSeconds  = 7 * 24 * 60 * 60
	videoCacheSeconds  = 10 * 60
	avatarCacheSeconds = 60 * 60
)

// Only content digests can name immutable media versions. Older archives may
// lack a digest; those responses must not get a long freshness lifetime.
func mediaVersion(hash string) string {
	if len(hash) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return ""
	}
	return strings.ToLower(hash)
}

func setPrivateCache(w http.ResponseWriter, etag string, seconds int) {
	w.Header().Set("ETag", etag)
	w.Header().Add("Vary", "Cookie") // Do not reuse a logged-in response in another session.
	if seconds > 0 {
		w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d, must-revalidate", seconds))
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
}

func (a *App) setMediaCache(w http.ResponseWriter, r *http.Request, hash, ext string) {
	version := mediaVersion(hash)
	if version == "" {
		return
	}
	seconds := 0
	// Old, unversioned links remain usable but revalidate on every reuse.
	if r.URL.Query().Get("v") == version {
		cfg := a.Config()
		seconds = cfg.ImageCacheSeconds
		if seconds <= 0 {
			seconds = imageCacheSeconds
		}
		if ext == ".mp4" {
			seconds = cfg.VideoCacheSeconds
			if seconds <= 0 {
				seconds = videoCacheSeconds
			}
		}
	}
	setPrivateCache(w, `"sha256-`+version+`"`, seconds)
}

// The archive DB already contains the content digest. A matching conditional
// GET/HEAD can be answered after authentication, without contacting S3/WebDAV.
// Other preconditions are left to ServeContent to preserve their ordering.
func mediaNotModified(w http.ResponseWriter, r *http.Request) bool {
	etag := w.Header().Get("ETag")
	if etag == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("If-Match") != "" || r.Header.Get("If-Unmodified-Since") != "" {
		return false
	}
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

// Never cache failures, including ServeContent's 412/416 and upstream errors.
// ServeContent normally removes Cache-Control on errors; explicitly restore
// no-store instead of leaving room for heuristic negative caching.
type mediaCacheResponseWriter struct{ http.ResponseWriter }

func (w *mediaCacheResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *mediaCacheResponseWriter) WriteHeader(status int) {
	if status >= 400 {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Del("ETag")
		w.Header().Del("Last-Modified")
	}
	w.ResponseWriter.WriteHeader(status)
}
