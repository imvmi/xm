package web

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPWAResources(t *testing.T) {
	for name, handler := range map[string]http.Handler{"app": testApp(t).Handler(), "setup": setupFixture(t).Handler()} {
		t.Run(name, func(t *testing.T) {
			get := func(path, contentType string) []byte {
				t.Helper()
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), contentType) {
					t.Fatalf("%s: %d %v", path, w.Code, w.Header())
				}
				return w.Body.Bytes()
			}
			var manifest struct {
				ID, Scope, Display string
				StartURL           string `json:"start_url"`
				Icons              []struct{ Src, Sizes, Purpose string }
			}
			if err := json.Unmarshal(get("/manifest.webmanifest", "application/manifest+json"), &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.ID != "/" || manifest.Scope != "/" || manifest.StartURL != "/" || manifest.Display != "standalone" || len(manifest.Icons) != 3 {
				t.Fatalf("invalid manifest: %+v", manifest)
			}
			for _, icon := range manifest.Icons {
				cfg, err := png.DecodeConfig(bytes.NewReader(get(icon.Src, "image/png")))
				if err != nil || cfg.Width != cfg.Height || (cfg.Width != 192 && cfg.Width != 512) {
					t.Fatalf("invalid icon %s: %+v %v", icon.Src, cfg, err)
				}
			}
			get("/assets/apple-touch-icon.png", "image/png")
			get("/offline.html", "text/html")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "/sw.js", nil))
			if w.Code != 200 || w.Header().Get("Service-Worker-Allowed") != "/" || w.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || strings.Contains(w.Body.String(), "__XM_REVISION__") || !strings.Contains(w.Body.String(), pwaRevision) {
				t.Fatalf("invalid service worker response: %d %v", w.Code, w.Header())
			}
		})
	}
}
