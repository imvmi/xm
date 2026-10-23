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

// Exercise native browser fetch/AbortController and actual HTTP disconnects.
// Database cancellation is tested separately with an occupied reader pool.
func TestTimelineRequestCancellationBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome request cancellation fixture")
	}
	a := testApp(t)
	started, canceled := make(chan struct{}, 4), make(chan struct{}, 4)
	handler := a.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/posts" && r.URL.Query().Get("q") == "slow" {
			started <- struct{}{}
			<-r.Context().Done()
			canceled <- struct{}{}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	alloc, closeAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer closeAlloc()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	eval := func(js string) bool {
		t.Helper()
		v, err := chromedp.Run(browser, chromedp.Evaluate[bool](js))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	wait := func(js string) {
		t.Helper()
		for !eval(js) {
			select {
			case <-ctx.Done():
				t.Fatal("UI timeout", js)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	expect := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal(what)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !$('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated && !state.postsLoading`)
	search := func(value string) {
		t.Helper()
		raw, _ := json.Marshal(value)
		eval(`(()=>{$('search').value=` + string(raw) + `;$('search').dispatchEvent(new Event('input'));return true})()`)
	}
	search("slow")
	expect(started, "search did not reach server")
	search("fast")
	expect(canceled, "changing search did not abort HTTP request")
	wait(`!state.postsLoading && $('search').value==='fast'`)
	if !eval(`!state.postsError && $('toast').hidden`) {
		t.Fatal("aborted search displayed error")
	}
	search("slow")
	expect(started, "second search did not reach server")
	eval(`(()=>{location.hash='settings';return true})()`)
	expect(canceled, "navigation did not abort HTTP request")
	wait(`state.view==='settings' && !$('view-settings').hidden`)
	if !eval(`$('toast').hidden`) {
		t.Fatal("navigation cancellation displayed error")
	}
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && !state.postsLoading`)
	search("slow")
	expect(started, "third search did not reach server")
	eval(`(()=>{$('logout').click();return true})()`)
	expect(canceled, "logout did not abort HTTP request")
	wait(`!state.authenticated && !$('login-view').hidden`)
}
