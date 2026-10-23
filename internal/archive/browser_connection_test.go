package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBrowserConnectionFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", syscall.ECONNREFUSED), "连接被拒绝"},
		{context.DeadlineExceeded, "超时"},
		{context.Canceled, "已取消"},
		{syscall.ENETUNREACH, "不可达"},
		{syscall.ECONNRESET, "提前关闭"},
		{fmt.Errorf("lost connection: %w: %w", io.EOF, context.Canceled), "提前关闭"},
		{errors.New(`could not dial ws://secret: websocket handshake failed: unexpected status "403 Forbidden", body "secret-body"`), "HTTP 403"},
	} {
		got := browserConnectionReason(tc.err)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "secret") {
			t.Fatalf("unexpected reason: %s", got)
		}
	}
}

func TestCDPDiagnosticFailureDetails(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"authorization", 401, "secret-body", "HTTP 401"},
		{"wrong port", 404, "secret-body", "HTTP 404"},
		{"management page", 200, "<html>secret-body</html>", "无效响应"},
		{"missing websocket", 200, `{}`, "缺少有效"},
		{"websocket denied", 200, `{"webSocketDebuggerUrl":"ws://localhost/devtools/browser/secret-token"}`, "WebSocket 握手被拒绝（HTTP 403）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/json/version" {
					w.WriteHeader(403)
					fmt.Fprint(w, "secret-body")
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := CheckChrome(ctx, cdpFixtureConfig(t, s.URL), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unexpected diagnostic error: %v", err)
			}
		})
	}
	s := httptest.NewServer(http.NotFoundHandler())
	cfg := cdpFixtureConfig(t, s.URL)
	s.Close()
	if _, err := CheckChrome(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "连接被拒绝") {
		t.Fatalf("missing refusal reason: %v", err)
	}
}

func cdpFixtureConfig(t *testing.T, endpoint string) Config {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.BrowserMode = "cdp"
	c.CDPHost = host
	c.CDPPort, _ = strconv.Atoi(port)
	return c
}

func TestCDPDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"browser", `{"webSocketDebuggerUrl":"ws://127.0.0.1:5818/devtools/browser/fixture"}`, 200, true},
		{"management page", "<html>management</html>", 200, false},
		{"missing websocket", `{}`, 200, false},
		{"wrong type", `{"webSocketDebuggerUrl":42}`, 200, false},
		{"page websocket", `{"webSocketDebuggerUrl":"ws://127.0.0.1/devtools/page/fixture"}`, 200, false},
		{"wrong scheme", `{"webSocketDebuggerUrl":"http://127.0.0.1/devtools/browser/fixture"}`, 200, false},
		{"unauthorized", `{}`, 401, false},
		{"redirect", `{}`, 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/json/version" {
					t.Error(r.URL.Path)
				}
				w.Header().Set("Location", "http://unreachable.invalid")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := cdpFixtureConfig(t, server.URL)
			cfg.Proxy = "http://unreachable.invalid:1"
			got, err := browserWebSocket(context.Background(), cfg)
			if (err == nil) != tc.ok {
				t.Fatalf("got %q %v", got, err)
			}
			if tc.ok && got != "ws://"+net.JoinHostPort(cfg.CDPHost, strconv.Itoa(cfg.CDPPort))+"/devtools/browser/fixture" {
				t.Fatal("advertised address replaced configured host", got)
			}
		})
	}
}

func TestCDPDiscoveryCancellation(t *testing.T) {
	ended := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(ended) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := browserWebSocket(ctx, cdpFixtureConfig(t, s.URL)); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("missing timeout reason: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("CDP discovery request leaked")
	}
}

func TestBrowserSettingsUpgradeAndReopen(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	s, err := OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"browser_mode", "cdp_host", "cdp_port"} {
		if _, err = s.DB.Exec("ALTER TABLE system_settings DROP COLUMN " + name); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSettings(cfg)
	if err != nil || got.BrowserMode != "local" || got.CDPHost != "127.0.0.1" || got.CDPPort != 9222 {
		t.Fatalf("upgrade: %+v %v", got, err)
	}
	got.BrowserMode = "cdp"
	got.CDPHost = "::1"
	got.CDPPort = 5818
	if err = s.SaveSettings(got); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadSettings(cfg)
	if err != nil || loaded.BrowserMode != "cdp" || loaded.CDPHost != "::1" || loaded.CDPPort != 5818 {
		t.Fatal("reopen lost CDP config", err)
	}
}
