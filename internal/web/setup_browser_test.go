package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Real Chrome against isolated local fixtures; never touches the running service.
func TestSetupBrowserNewAndExisting(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("set XM_BROWSER_TEST=1 for setup UI integration")
	}
	for _, existing := range []bool{false, true} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			s := setupFixture(t)
			input := setupValid()
			if existing {
				s, _, input = existingSetupFixture(t)
			}
			handler := s.Handler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/setup/status" && len(s.done) != 0 {
					respond(w, 200, map[string]bool{"needed": false})
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			alloc, stopAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
			defer stopAlloc()
			browser, stopBrowser := chromedp.NewContext(alloc)
			defer stopBrowser()
			eval := func(js string) bool {
				t.Helper()
				value, err := chromedp.Run(browser, chromedp.Evaluate[bool](js))
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			wait := func(js string) {
				t.Helper()
				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()
				for !eval(js) {
					select {
					case <-ctx.Done():
						t.Fatal("setup UI timed out", js)
					case <-ticker.C:
					}
				}
			}
			fill := func(values map[string]any) {
				t.Helper()
				data, err := json.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				eval(`(()=>{for(const [id,value] of Object.entries(` + string(data) + `))document.getElementById(id).value=value;return true})()`)
			}
			if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
				t.Fatal(err)
			}
			wait(`typeof loaded!=='undefined' && loaded && !busy`)
			for _, width := range []int64{1280, 390} {
				if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
					t.Fatal(err)
				}
				eval(`(()=>{document.querySelector('input[name="db-kind"][value="mariadb"]').click();return true})()`)
				if !eval(`!el('mysql-fields').hidden && !el('mysql-fields').disabled && el('sqlite-note').hidden && database().driver==='mariadb' && document.documentElement.scrollWidth<=innerWidth`) {
					t.Fatal("MariaDB setup fields missing or overflowing")
				}
			}
			if !existing {
				eval(`(()=>{document.querySelector('input[name="db-kind"][value="sqlite"]').click();return true})()`)
			}
			if existing {
				eval(`(()=>{document.querySelector('input[name="db-kind"][value="mysql"]').click();return true})()`)
				d := input.Database
				fill(map[string]any{"db-host": d.Host, "db-port": d.Port, "db-name": d.Name, "db-user": d.User, "db-password": d.Password, "db-tls": d.TLSMode})
			}
			eval(`(()=>{document.getElementById('setup-next').click();return true})()`)
			if existing {
				wait(`!busy && !document.getElementById('step-existing').hidden`)
				fill(map[string]any{"existing-username": input.Username, "existing-password": "wrong-password"})
				eval(`(()=>{document.getElementById('setup-next').click();return true})()`)
				wait(`!busy && document.getElementById('setup-error').textContent.includes('密码不正确')`)
				fill(map[string]any{"existing-password": input.Password})
			} else {
				wait(`!busy && !document.getElementById('step-storage').hidden`)
				eval(`(()=>{el('media-kind').value='smb';el('media-kind').dispatchEvent(new Event('change'));el('media-endpoint').value='smb://nas/archive';return true})()`)
				if !eval(`!el('remote-fields').hidden && !el('webdav-fields').hidden && el('s3-fields').hidden && el('media-user').required && el('media-password').required && el('media-endpoint').checkValidity() && el('media-endpoint-help').textContent.includes('445')`) {
					t.Fatal("Samba setup fields or URL validation incorrect")
				}
				eval(`(()=>{el('media-kind').value='local';el('media-kind').dispatchEvent(new Event('change'));return true})()`)
				eval(`(()=>{document.getElementById('setup-next').click();return true})()`)
				wait(`!busy && !document.getElementById('step-account').hidden`)
				fill(map[string]any{"setup-username": input.Username, "setup-password": input.Password, "setup-confirm": input.Password})
			}
			eval(`(()=>{document.getElementById('setup-next').click();return true})()`)
			wait(`!document.getElementById('setup-success').hidden && !document.getElementById('setup-login').disabled`)
			if len(s.done) != 1 || !eval(`Array.from(document.querySelectorAll('input[type="password"]')).every(input=>input.value==='')`) {
				t.Fatal("setup did not finish or clear password fields")
			}
		})
	}
}
