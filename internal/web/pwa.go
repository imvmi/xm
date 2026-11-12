package web

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// Include every embedded asset in the revision so releases invalidate the worker
// without requiring a manually maintained version or a separate frontend build.
var pwaRevision = func() string {
	h := sha256.New()
	_ = fs.WalkDir(static, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		b, err := static.ReadFile(path)
		if err == nil {
			fmt.Fprintf(h, "%s\x00%d\x00", path, len(b))
			_, _ = h.Write(b)
		}
		return err
	})
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}()

func registerPWA(mux *http.ServeMux) {
	for route, asset := range map[string]string{
		"/manifest.webmanifest": "manifest.webmanifest",
		"/sw.js":                "sw.js",
		"/offline.html":         "offline.html",
	} {
		mux.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
			b, _ := static.ReadFile("static/" + asset)
			switch asset {
			case "sw.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				w.Header().Set("Service-Worker-Allowed", "/")
				b = []byte(strings.ReplaceAll(string(b), "__XM_REVISION__", pwaRevision))
			case "manifest.webmanifest":
				w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
			default:
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
			}
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(b)
		})
	}
}
