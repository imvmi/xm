package archive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// Stop at a real CDP protocol boundary, including after the isolated context
// has been created but before its first target exists. No installed Chrome or
// external network is needed to exercise timeout, cancellation and disconnect.
func TestCDPInitializationLifecycle(t *testing.T) {
	for _, stage := range []string{"handshake", "create-context", "create-target"} {
		for _, ending := range []string{"cancel", "timeout", "disconnect"} {
			if stage == "handshake" && ending == "disconnect" {
				continue
			}
			t.Run(stage+"/"+ending, func(t *testing.T) {
				reached, released := make(chan struct{}), make(chan struct{})
				ws := websocket.Server{Handshake: func(*websocket.Config, *http.Request) error { return nil }, Handler: func(conn *websocket.Conn) {
					defer close(released)
					defer conn.Close()
					for {
						var msg struct {
							ID     int    `json:"id"`
							Method string `json:"method"`
							Params struct {
								DisposeOnDetach bool `json:"disposeOnDetach"`
							} `json:"params"`
						}
						if err := websocket.JSON.Receive(conn, &msg); err != nil {
							return
						}
						if msg.Method == "Target.createBrowserContext" && !msg.Params.DisposeOnDetach {
							t.Error("remote context will leak on disconnect")
						}
						stop := (stage == "create-context" && msg.Method == "Target.createBrowserContext") || (stage == "create-target" && msg.Method == "Target.createTarget")
						if stop {
							close(reached)
							if ending == "disconnect" {
								return
							}
							// Leave the command unanswered, but read until client closes.
							var ignored string
							for websocket.Message.Receive(conn, &ignored) == nil {
							}
							return
						}
						result := map[string]any{}
						if msg.Method == "Target.createBrowserContext" {
							result["browserContextId"] = "owned-fixture"
						}
						if err := websocket.JSON.Send(conn, map[string]any{"id": msg.ID, "result": result}); err != nil {
							return
						}
					}
				}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/json/version" {
						fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://localhost/devtools/browser/fixture"}`)
						return
					}
					if stage == "handshake" {
						close(reached)
						<-r.Context().Done()
						close(released)
						return
					}
					ws.ServeHTTP(w, r)
				}))
				defer server.Close()
				ctx, cancel := context.WithCancel(context.Background())
				if ending == "timeout" {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
				}
				defer cancel()
				done := make(chan error, 1)
				go func() { _, err := CheckChrome(ctx, cdpFixtureConfig(t, server.URL), nil); done <- err }()
				select {
				case <-reached:
				case <-time.After(3 * time.Second):
					t.Fatal("did not reach protocol stage")
				}
				if ending == "cancel" {
					cancel()
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("unfinished initialization reported success")
					}
					if ending == "cancel" && !errors.Is(err, context.Canceled) {
						t.Fatalf("lost cancellation: %v", err)
					}
					if ending == "timeout" && (!errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "超时")) {
						t.Fatalf("lost deadline: %v", err)
					}
					if ending == "disconnect" && !strings.Contains(err.Error(), "断开连接") && !strings.Contains(err.Error(), "提前关闭") {
						t.Fatalf("disconnect mislabeled: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("diagnostic failed to stop")
				}
				select {
				case <-released:
				case <-time.After(2 * time.Second):
					t.Fatal("diagnostic leaked connection")
				}
			})
		}
	}
}

func TestCDPDiscoveryBodyDeadline(t *testing.T) {
	released := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"webSocketDebuggerUrl":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(released)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := browserWebSocket(ctx, cdpFixtureConfig(t, s.URL))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("body timeout lost: %v", err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("stalled body leaked request")
	}
}
