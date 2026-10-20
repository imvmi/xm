package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestTimelinePrefetchBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	cfg := a.Config()
	cfg.TimelinePageSize = 10
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var posts []archive.Post
	for i := 0; i < 30; i++ {
		posts = append(posts, archive.Post{ID: fmt.Sprint(100 + i), Username: "alice", Text: "预加载测试", Raw: json.RawMessage(`{}`)})
	}
	if err := a.store.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
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
		deadline := time.Now().Add(8 * time.Second)
		for !eval(js) {
			if time.Now().After(deadline) {
				t.Fatal("timed out", js)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 700), chromedp.Navigate(server.URL+"/#settings")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`state.authenticated && state.view==='settings'`)
	// Tall cards make the three-post trigger unambiguously earlier than the
	// old fixed 320px distance, without downloading any real media.
	eval(`(()=>{const sheet=document.styleSheets[0];sheet.insertRule('#posts>.post{min-height:400px}',sheet.cssRules.length);const original=window.fetch;window.prefetchSamples=[];window.fetch=function(url,...args){if(String(url).includes('/posts?')&&String(url).includes('before='))prefetchSamples.push({gap:$('timeline-loader').getBoundingClientRect().top-innerHeight,count:$('posts').children.length});return original.call(this,url,...args)};return true})()`)
	for _, route := range []string{"home", "user/alice"} {
		eval(fmt.Sprintf(`(()=>{location.hash=%q;return true})()`, route))
		view := "home"
		if route != "home" {
			view = "user"
		}
		wait(fmt.Sprintf(`state.view===%q && !state.postsLoading && state.postIDs.size===10`, view))
		if !eval(`(()=>{window.prefetchSamples=[];window.firstPrefetchPost=$('posts').firstElementChild;firstPrefetchPost.dataset.preserved='true';const target=$('posts').children[7];window.scrollTo({top:scrollY+target.getBoundingClientRect().top-innerHeight+100,behavior:'instant'});return $('timeline-loader').getBoundingClientRect().top-innerHeight>320})()`) {
			t.Fatal("fixture already near bottom", route)
		}
		wait(`!state.postsLoading && state.postIDs.size===20`)
		if !eval(`prefetchSamples.length===1 && prefetchSamples[0].count===10 && prefetchSamples[0].gap>320 && $('posts').firstElementChild===firstPrefetchPost && firstPrefetchPost.dataset.preserved==='true'`) {
			t.Fatal("prefetch was late, duplicated, or replaced existing content", route)
		}
	}
}
