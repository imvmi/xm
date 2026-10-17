package archive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCookieSourcePersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := CookieSource{URL: "https://example.com/info/x", BearerToken: "private-token", AutoRefresh: true, UseProxy: true}
	if err = s.SaveCookieSource(want); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.LoadCookieSource()
	if err != nil || got != want {
		t.Fatalf("source not persisted: %v", err)
	}
}

func TestFetchCookies(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"map", `{"auth_token":"auth-value","ct0":"csrf-value"}`, 200, true},
		{"array", `[{"name":"auth_token","value":"auth-value","domain":".x.com"},{"name":"ct0","value":"csrf-value","domain":".x.com"}]`, 200, true},
		{"missing", `{"auth_token":"auth-value"}`, 200, false},
		{"invalid", `<html>private upstream error</html>`, 200, false},
		{"empty", "", 200, false},
		{"null", "null", 200, false},
		{"unauthorized", "private upstream error", 401, false},
		{"oversize", strings.Repeat("x", (1<<20)+1), 200, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer source-secret" || r.Header.Get("Cookie") != "" {
					t.Error("incorrect request authentication")
				}
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			got, err := FetchCookies(context.Background(), CookieSource{URL: srv.URL, BearerToken: "source-secret"}, "")
			if (err == nil) != tt.valid {
				t.Fatalf("unexpected result: %v", err)
			}
			if err == nil && (got.AuthToken != "auth-value" || got.CSRF != "csrf-value") {
				t.Fatal("wrong cookies")
			}
			if err != nil && (strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "source-secret")) {
				t.Fatal("upstream secret leaked")
			}
		})
	}
}

func TestCookieSourceRedirectAndCancellation(t *testing.T) {
	redirected := false
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer dest.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, http.StatusFound) }))
	defer srv.Close()
	source := CookieSource{URL: srv.URL, BearerToken: "source-secret"}
	if _, err := FetchCookies(context.Background(), source, ""); err == nil || redirected {
		t.Fatal("followed redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchCookies(ctx, source, ""); err == nil {
		t.Fatal("ignored cancellation")
	}
	for _, endpoint := range []string{"file:///tmp/cookies", "https://user:password@example.com/x", "https://example.com/#secret", "https:///no-host"} {
		if err := (CookieSource{URL: endpoint}).Validate(); err == nil {
			t.Fatal("accepted invalid URL")
		}
	}
	if err := (CookieSource{URL: srv.URL, BearerToken: "bad\r\nheader"}).Validate(); err == nil {
		t.Fatal("accepted header injection")
	}
}
