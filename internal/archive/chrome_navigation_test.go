package archive

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestChromeDiagnosticSlowImage(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome diagnostic fixture")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow.png" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><button data-testid="SideNav_AccountSwitcher_Button">Account</button><img src="/slow.png"></body></html>`))
	}))
	defer server.Close()
	for _, fullLoad := range []bool{true, false} {
		name := "document"
		if fullLoad {
			name = "full_load"
		}
		t.Run(name, func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			alloc, stopAlloc := chromedp.NewExecAllocator(parent, chromedp.DefaultExecAllocatorOptions[:]...)
			defer stopAlloc()
			ctx, stop := chromedp.NewContext(alloc)
			defer stop()
			if err := chromedp.Do(ctx); err != nil {
				t.Fatal(err)
			}
			check, stopCheck := context.WithTimeout(ctx, 2*time.Second)
			defer stopCheck()
			var err error
			if fullLoad {
				err = chromedp.Do(check, chromedp.Navigate(server.URL))
			} else {
				err = navigateChromeDocument(check, server.URL)
			}
			if fullLoad {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expected full-load timeout, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = confirmSession(check); err != nil {
					t.Fatal(err)
				}
				ready, err := chromedp.Run(check, chromedp.Evaluate[string](`document.readyState`))
				if err != nil || ready != "interactive" {
					t.Fatalf("expected incomplete asset loading: ready=%s err=%v", ready, err)
				}
			}
		})
	}
}

func TestChromeDiagnosticDocumentFailures(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome diagnostic fixture")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stalled":
			<-r.Context().Done()
		case "/redirect":
			http.Redirect(w, r, "/i/flow/login", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>Login required</body></html>`))
		}
	}))
	defer server.Close()
	for _, path := range []string{"/stalled", "/redirect"} {
		t.Run(path, func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			alloc, stopAlloc := chromedp.NewExecAllocator(parent, chromedp.DefaultExecAllocatorOptions[:]...)
			defer stopAlloc()
			ctx, stop := chromedp.NewContext(alloc)
			defer stop()
			if err := chromedp.Do(ctx); err != nil {
				t.Fatal(err)
			}
			check, stopCheck := context.WithTimeout(ctx, 2*time.Second)
			defer stopCheck()
			err := navigateChromeDocument(check, server.URL+path)
			if path == "/stalled" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("missing document must time out: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = confirmSession(check); FailureKind(err) != "auth" {
					t.Fatalf("login redirect must fail authentication: %v", err)
				}
			}
		})
	}
}
