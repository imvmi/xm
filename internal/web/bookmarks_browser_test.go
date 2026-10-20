package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestBookmarksBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	quote := archive.Post{ID: "50", Username: "bob", Text: "这是一条可单独收藏的引用推文。"}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", Username: "alice", Text: "点击右上角书签收藏这条动态。", Quote: &quote}}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("bob", []archive.Post{{ID: "200", Username: "bob", Text: "另一个账号的动态。"}}); err != nil {
		t.Fatal(err)
	}
	seedMonitor(t, a, "alice")
	var reject atomic.Bool
	handler := a.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject.Load() && r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/bookmark") {
			problem(w, 500, "测试保存失败")
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
				t.Fatal("timed out", js)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	click := func(selector string) {
		t.Helper()
		eval(fmt.Sprintf(`(()=>{document.querySelector(%q).click();return true})()`, selector))
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`state.authenticated && !state.postsLoading && state.postIDs.size===2`)
	if !eval(`state.view==='home' && state.timelineTab==='' && $('home-tab-all').getAttribute('aria-selected')==='true'`) {
		t.Fatal("home did not default to all")
	}
	click(`[data-bookmark-id="100"]`)
	wait(`document.querySelector('[data-bookmark-id="100"]').getAttribute('aria-pressed')==='true'`)
	click(`[data-bookmark-id="50"]`)
	wait(`document.querySelector('[data-bookmark-id="50"]').getAttribute('aria-pressed')==='true'`)
	click("#home-tab-bookmarks")
	wait(`state.timelineTab==='bookmarks' && !state.postsLoading && state.postIDs.size===2`)
	if !eval(`$('posts').firstElementChild.dataset.postId==='50' && [...document.querySelectorAll('[data-bookmark-id="50"]')].every(b=>b.getAttribute('aria-pressed')==='true')`) {
		t.Fatal("bookmark ordering or shared quote state wrong")
	}
	eval(`(()=>{location.hash='user/alice';return true})()`)
	wait(`state.view==='user' && state.target==='alice' && !state.postsLoading && state.timelineTab==='posts'`)
	if !eval(`$('tab-media').nextElementSibling===$('tab-bookmarks')`) {
		t.Fatal("account bookmark tab not after media")
	}
	// Keyboard navigation from Media must reach Bookmarks, then wrap to Posts.
	eval(`(()=>{$('tab-media').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowRight',bubbles:true}));return true})()`)
	wait(`state.timelineTab==='bookmarks' && !state.postsLoading && state.postIDs.size===2`)
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
				t.Fatal(err)
			}
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`document.documentElement.scrollWidth<=innerWidth && [...document.querySelectorAll('#posts .bookmark-button')].every(b=>b.getBoundingClientRect().width>=40)`) {
				t.Fatalf("bookmark layout failed %s %d", theme, width)
			}
		}
	}
	if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
		eval(`(()=>{$('tab-bookmarks').scrollIntoView({behavior:'instant',block:'start'});return true})()`)
		if err := chromedp.Do(browser, chromedp.Sleep(250*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "bookmarks-dark-mobile.png"), shot, 0600); err != nil {
			t.Fatal(err)
		}
	}
	click(`article[data-post-id="100"] > .post-body .post-header [data-bookmark-id="100"]`)
	wait(`!state.postsLoading && state.postIDs.size===1 && !state.postIDs.has('100') && state.total===1`)
	eval(`(()=>{location.hash='user/bob';return true})()`)
	wait(`state.target==='bob' && state.timelineTab==='posts' && !state.postsLoading`)
	click("#tab-bookmarks")
	wait(`state.timelineTab==='bookmarks' && !state.postsLoading && state.postIDs.size===0`)
	if !eval(`!$('post-empty').hidden && $('post-empty').textContent.includes('还没有收藏')`) {
		t.Fatal("account bookmark scope or empty state wrong")
	}
	click("#tab-posts")
	wait(`state.timelineTab==='posts' && !state.postsLoading && state.postIDs.has('200')`)
	click(`[data-bookmark-id="200"]`)
	wait(`document.querySelector('[data-bookmark-id="200"]').getAttribute('aria-pressed')==='true'`)
	if _, err := a.store.DB.Exec("UPDATE monitors SET show_in_timeline=0 WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && state.timelineTab==='' && !state.postsLoading && state.postIDs.size===1`)
	click("#home-tab-bookmarks")
	wait(`state.timelineTab==='bookmarks' && !state.postsLoading && state.postIDs.size===2 && state.postIDs.has('50')`)
	reject.Store(true)
	click(`[data-bookmark-id="200"]`)
	wait(`$('toast').textContent==='测试保存失败' && !document.querySelector('[data-bookmark-id="200"]').disabled`)
	if !eval(`state.postIDs.has('200') && document.querySelector('[data-bookmark-id="200"]').getAttribute('aria-pressed')==='true'`) {
		t.Fatal("failed save changed bookmark UI")
	}
	reject.Store(false)
	if err := chromedp.Do(browser, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && state.authenticated && state.view==='home' && !state.postsLoading && state.postIDs.size===1`)
	if !eval(`state.timelineTab==='' && document.querySelector('[data-bookmark-id="200"]').getAttribute('aria-pressed')==='true'`) {
		t.Fatal("reload lost bookmark or default tab")
	}
	click("#home-tab-bookmarks")
	wait(`state.timelineTab==='bookmarks' && !state.postsLoading && state.postIDs.size===2`)
}
