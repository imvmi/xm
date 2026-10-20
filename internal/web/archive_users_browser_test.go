package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestArchiveDirectoryLoadsBeforeStatisticsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	for _, name := range []string{"alice", "bob"} {
		if err := a.store.SavePosts(name, []archive.Post{{ID: name + "1", Username: name, Text: "目录测试"}}); err != nil {
			t.Fatal(err)
		}
	}
	handler := a.Handler()
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/users/stats" || r.URL.Path == "/api/overview" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	alloc, stopAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer stopAlloc()
	browser, stopBrowser := chromedp.NewContext(alloc)
	defer stopBrowser()
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
		deadline := time.Now().Add(8 * time.Second)
		for !eval(js) {
			if time.Now().After(deadline) {
				t.Fatal("UI timed out:", js)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#archive")); err != nil {
		t.Fatal(err)
	}
	wait(`!document.getElementById('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`document.querySelectorAll('#user-list .user-card').length===2`)
	if !eval(`state.users.every(u=>u.stats_pending) && $('user-list').textContent.includes('—') && $('user-empty').hidden && !$('user-status').hidden`) {
		t.Fatal("directory must be usable before any statistics response")
	}
	// Account timelines must also be usable while statistics remain blocked.
	eval(`(()=>{location.hash='user/alice';return true})()`)
	wait(`state.view==='user' && state.postIDs.size===1 && !state.postsLoading`)
	unblock()
	wait(`state.users.length===1 && !state.users[0].stats_pending`)
	eval(`(()=>{location.hash='archive';return true})()`)
	wait(`document.querySelectorAll('#user-list .user-card').length===2 && state.users.every(u=>!u.stats_pending)`)
	if !eval(`$('user-list').textContent.includes('alice') && $('user-list').textContent.includes('bob')`) {
		t.Fatal("account detail replaced full directory")
	}
}
