package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPostReadsCancelWhileWaitingForConnection(t *testing.T) {
	a := testApp(t)
	pool := a.store.DB.Reader
	if pool == nil {
		pool = a.store.DB.DB
	}
	pool.SetMaxOpenConns(1)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for name, read := range map[string]func(context.Context) error{
		"timeline": func(ctx context.Context) error {
			_, err := listPostsPage(ctx, a.store.DB, "alice", "中文", "video", "", 0, 24, "", false)
			return err
		},
		"count": func(ctx context.Context) error {
			_, err := listPostsPage(ctx, a.store.DB, "alice", "中文", "", "", 0, 24, "", true)
			return err
		},
		"bookmarks": func(ctx context.Context) error {
			_, err := listPostsPage(ctx, a.store.DB, "alice", "中文", "photo", "bookmarks", 0, 24, "", false)
			return err
		},
		"quotes": func(ctx context.Context) error {
			return attachPosts(ctx, a.store.DB, []PostView{{ID: "2", QuoteID: "1"}})
		},
		"media": func(ctx context.Context) error { return attachPosts(ctx, a.store.DB, []PostView{{ID: "2"}}) },
		"reply previews": func(ctx context.Context) error {
			return attachReplyPreviews(ctx, a.store.DB, []PostView{{ID: "2", ReplyTo: "1"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- read(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("query ignored cancellation while pool was occupied")
			}
		})
	}
}

func TestPostReadHTTPDeadlines(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	_, token := createTestCredential(t, a, fullTestCredential())
	pool := a.store.DB.Reader
	if pool == nil {
		pool = a.store.DB.DB
	}
	pool.SetMaxOpenConns(1)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, path := range []string{"/api/posts?count=0", "/api/posts?section=bookmarks", "/api/posts/1", "/api/posts/1/context", "/api/posts/1/replies", "/api/v1/accounts/alice/timeline", "/api/v1/posts/1"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
			r.AddCookie(b.cookie)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { a.Handler().ServeHTTP(w, r); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not return after deadline")
			}
			if w.Code != 504 || !strings.Contains(w.Body.String(), "查询超时") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if strings.HasPrefix(path, "/api/v1/") && !strings.Contains(w.Body.String(), `"code":"query_timeout"`) {
				t.Fatal("external error envelope lost")
			}
		})
	}
}

func TestPostReadHTTPDisconnectReleasesQuery(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	pool := a.store.DB.Reader
	if pool == nil {
		pool = a.store.DB.DB
	}
	pool.SetMaxOpenConns(1)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan struct{})
	handler := a.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r); close(done) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/posts?count=0&q=needle", nil)
	r.AddCookie(b.cookie)
	clientDone := make(chan error, 1)
	waits := pool.Stats().WaitCount
	go func() {
		resp, err := http.DefaultClient.Do(r)
		if resp != nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for pool.Stats().WaitCount == waits && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pool.Stats().WaitCount == waits {
		t.Fatal("HTTP request never reached database")
	}
	cancel()
	select {
	case err := <-clientDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not cancel")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected request still occupies server")
	}
	conn.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := listPostsPage(ctx2, a.store.DB, "", "", "", "", 0, 24, "", false); err != nil {
		t.Fatal("pool did not recover", err)
	}
}

func TestReadTimeoutDoesNotPublishPartialSuccess(t *testing.T) {
	w := httptest.NewRecorder()
	readTimeout(time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]string{"data": "partial"})
		<-r.Context().Done()
	})(w, httptest.NewRequest("GET", "/api/posts", nil))
	if w.Code != 504 || strings.Contains(w.Body.String(), "partial") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestReadTimeoutPreservesRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "request-123")
	readTimeout(time.Second, func(w http.ResponseWriter, r *http.Request) { apiError(w, 404, "not_found", "missing") })(w, httptest.NewRequest("GET", "/api/v1/posts/1", nil))
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"request_id":"request-123"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
