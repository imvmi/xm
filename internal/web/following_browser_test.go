package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestFollowingBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome follow fixture")
	}
	a := testApp(t)
	for i, target := range []string{"alice", "bob"} {
		if err := a.store.SavePosts(target, []archive.Post{{ID: fmt.Sprint(100 + i), Username: target, Text: target + " 的动态"}}); err != nil {
			t.Fatal(err)
		}
	}
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
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated && !state.postsLoading && state.postIDs.size===2`)
	eval(`(()=>{$('home-tab-following').click();return true})()`)
	wait(`state.timelineTab==='following' && !state.postsLoading && state.postIDs.size===0 && $('post-empty').hidden===false`)
	if !eval(`$('home-tab-following').getAttribute('aria-selected')==='true' && $('empty-text').textContent.includes('关注')`) {
		t.Fatal("following tab or empty state")
	}
	eval(`(()=>{location.hash='user/alice';return true})()`)
	wait(`state.view==='user' && state.target==='alice' && !state.postsLoading && !!$('profile-follow')`)
	if !eval(`$('profile-follow').textContent==='关注' && $('profile-follow').getAttribute('aria-pressed')==='false'`) {
		t.Fatal("initial follow state")
	}
	// Hold a failed request to verify double clicks and background profile renders.
	eval(`(()=>{window.originalFollowFetch=window.fetch;window.followSaves=0;window.fetch=(url,...args)=>{if(String(url).endsWith('/follow')){followSaves++;return new Promise(resolve=>window.finishFollow=()=>resolve(new Response(JSON.stringify({error:'fixture follow failure'}),{status:500})))}return originalFollowFetch(url,...args)};$('profile-follow').click();renderProfile();$('profile-follow').click();return true})()`)
	if !eval(`followSaves===1 && $('profile-follow').disabled`) {
		t.Fatal("duplicate follow request")
	}
	eval(`(()=>{finishFollow();return true})()`)
	wait(`!state.followingPending.size && $('toast').textContent==='fixture follow failure'`)
	if !eval(`$('profile-follow').getAttribute('aria-pressed')==='false' && !$('profile-follow').disabled`) {
		t.Fatal("failed follow changed state")
	}
	eval(`(()=>{window.fetch=window.originalFollowFetch;$('profile-follow').click();return true})()`)
	wait(`!state.followingPending.size && $('profile-follow').getAttribute('aria-pressed')==='true'`)
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#user/alice")); err != nil {
		t.Fatal(err)
	}
	wait(`state.authenticated && !state.postsLoading && $('profile-follow')?.getAttribute('aria-pressed')==='true'`)
	for _, width := range []int64{1280, 390, 320} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
			t.Fatal(err)
		}
		for _, theme := range []string{"light", "dark"} {
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`(()=>{const b=$('profile-follow').getBoundingClientRect(),a=document.querySelector('.profile-avatar').getBoundingClientRect();return b.left>=a.right && b.right<=innerWidth && b.width>=80 && b.height>=40 && document.documentElement.scrollWidth<=innerWidth})()`) {
				t.Fatal("follow action overlap or overflow", width, theme)
			}
			if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
				shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("follow-%s-%d.png", theme, width)), shot, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && !state.postsLoading`)
	eval(`(()=>{$('home-tab-following').click();return true})()`)
	wait(`!state.postsLoading && state.postIDs.size===1 && state.postIDs.has('100')`)
	eval(`(()=>{$('home-tab-following').focus();$('home-tab-following').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowRight',bubbles:true}));return true})()`)
	wait(`state.timelineTab==='bookmarks' && !state.postsLoading`)
	eval(`(()=>{$('home-tab-following').click();return true})()`)
	wait(`state.timelineTab==='following' && !state.postsLoading`)
	eval(`(()=>{location.hash='user/alice';return true})()`)
	wait(`state.view==='user' && !state.postsLoading && $('profile-follow')?.getAttribute('aria-pressed')==='true'`)
	eval(`(()=>{$('profile-follow').click();return true})()`)
	wait(`!state.followingPending.size && $('profile-follow').getAttribute('aria-pressed')==='false'`)
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && state.timelineTab==='following' && !state.postsLoading && state.postIDs.size===0`)
	eval(`(()=>{$('home-tab-all').click();return true})()`)
	wait(`!state.postsLoading && state.postIDs.size===2`)
}
