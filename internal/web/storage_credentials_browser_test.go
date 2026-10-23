package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestStorageCredentialsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome storage credential fixture")
	}
	a := testApp(t)
	var mu sync.Mutex
	objects := map[string]string{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		if u != "user" || p != "new-password" {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(201)
		case "PUT":
			data, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = string(data)
		case "GET", "HEAD":
			value, ok := objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, "probe", time.Time{}, strings.NewReader(value))
		case "DELETE":
			delete(objects, r.URL.Path)
			w.WriteHeader(204)
		}
	}))
	defer remote.Close()
	cfg := a.Config()
	cfg.StorageProfiles = []archive.StorageProfile{{ID: "history", Name: "历史媒体存储", Kind: "webdav", Endpoint: remote.URL, Username: "user", Password: "old-password", Timeout: 5}, {ID: "s3", Name: "S3", Kind: "s3", Region: "us-east-1", Bucket: "bucket", AccessKey: "old-key", SecretKey: "old-secret", Timeout: 5}}
	if err := a.store.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	a.config = cfg
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	alloc, closeAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer closeAlloc()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
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
				t.Fatal("UI timed out", js)
			case <-ticker.C:
			}
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !$('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`!$('view-settings').hidden && $('storage-credentials-profile').options.length===2`)
	eval(`(()=>{$('settings-tab-data').click();return true})()`)
	if !eval(`!$('storage-credentials-panel').hidden && $('storage-credentials-panel').getBoundingClientRect().height>0 && $('storage-credentials-profile').value==='history' && $('storage-credentials-username').value==='user' && $('storage-credentials-password').value==='' && $('storage-credentials-password').type==='password'`) {
		t.Fatal("historical credentials UI missing or leaked saved password")
	}
	eval(`(()=>{$('storage-credentials-password').value='bad-password';document.querySelector('#storage-credentials-form button[type=submit]').click();return true})()`)
	wait(`$('storage-credentials-result').textContent.includes('验证失败') && !document.querySelector('#storage-credentials-form button').disabled`)
	if !eval(`$('storage-credentials-password').value===''`) {
		t.Fatal("failed submit retained password")
	}
	eval(`(()=>{$('storage-credentials-password').value='new-password';document.querySelector('#storage-credentials-form button[type=submit]').click();return true})()`)
	wait(`$('storage-credentials-result').textContent.includes('已验证并更新') && !document.querySelector('#storage-credentials-form button').disabled`)
	p, _ := a.Config().Storage("history")
	if p.Password != "new-password" || a.Config().ActiveStorage() != "local" {
		t.Fatal("rotation did not preserve active storage")
	}
	eval(`(()=>{$('storage-credentials-profile').value='s3';$('storage-credentials-profile').dispatchEvent(new Event('change'));return true})()`)
	if !eval(`!$('storage-credentials-s3').hidden && $('storage-credentials-login').hidden && $('storage-credentials-access').value==='' && $('storage-credentials-secret').value===''`) {
		t.Fatal("provider fields or secrets incorrect")
	}
	eval(`(()=>{$('storage-credentials-secret').value='unsaved-secret';location.hash='home';return true})()`)
	wait(`state.view==='home' && $('storage-credentials-secret').value===''`)
}
