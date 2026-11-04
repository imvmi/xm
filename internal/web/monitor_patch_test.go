package web

import (
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExternalAccountPatchPreservesConcurrentChanges(t *testing.T) {
	for _, tc := range []struct{ name, patch, concurrentSQL string }{
		{"pause", `{"include_replies":true}`, "UPDATE monitors SET enabled=0 WHERE username='alice'"},
		{"reply_scope", `{"enabled":false}`, "UPDATE monitors SET replies=1 WHERE username='alice'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			r := httptest.NewRequest("PATCH", "/api/v1/accounts/alice", strings.NewReader(tc.patch))
			r.Header.Set("Content-Type", "application/json")
			r.SetPathValue("account", "alice")
			w := httptest.NewRecorder()
			// Hold the UI edit lock until PATCH reaches its mutation path. This
			// reproduces the old snapshot-before-lock race without a timing sleep.
			a.changeMu.Lock()
			done := make(chan struct{})
			go func() { a.externalEditAccount(w, r, apiCredential{}); close(done) }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				stack := string(buf[:n])
				if strings.Contains(stack, "xm/internal/web.(*App).updateMonitor(") || strings.Contains(stack, "xm/internal/web.(*App).editMonitor(") {
					break
				}
				if time.Now().After(deadline) {
					a.changeMu.Unlock()
					<-done
					t.Fatal("PATCH did not reach monitor edit", w.Body.String())
				}
				runtime.Gosched()
			}
			_, err := a.store.DB.Exec(tc.concurrentSQL)
			a.changeMu.Unlock()
			<-done
			if err != nil {
				t.Fatal(err)
			}
			ok(t, w, 200)
			var enabled, replies bool
			if err := a.store.DB.QueryRow("SELECT enabled,replies FROM monitors WHERE username='alice'").Scan(&enabled, &replies); err != nil {
				t.Fatal(err)
			}
			if enabled || !replies {
				t.Fatalf("PATCH overwrote a concurrent edit: enabled=%v replies=%v", enabled, replies)
			}
		})
	}
}
