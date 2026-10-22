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

func TestSelfReplyContextBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome self-reply fixture")
	}
	a := testApp(t)
	posts := []archive.Post{{ID: "100", AuthorID: "42", Username: "alice", Text: "第一条：这是这段内容的起点。", CreatedAt: "2026-10-22T08:00:00Z"}}
	for i := 101; i <= 124; i++ {
		posts = append(posts, archive.Post{ID: fmt.Sprint(i), AuthorID: "42", Username: "alice", ReplyTo: fmt.Sprint(i - 1), ReplyToAuthorID: "42", Text: fmt.Sprintf("第 %d 条补充：自回复也应出现在主时间线，原帖摘要帮助理解上下文。", i-99), CreatedAt: "2026-10-22T08:10:00Z"})
	}
	posts[22].Text = `<img src=x onerror="window.replyXSS=true"> literal text`
	posts = append(posts, archive.Post{ID: "200", AuthorID: "42", Username: "alice", ReplyTo: "199", ReplyToAuthorID: "42", Text: "这条回复的原帖尚未归档。", CreatedAt: "2026-10-22T08:20:00Z"})
	if err := a.store.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	alloc, stopAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer stopAlloc()
	browser, stop := chromedp.NewContext(alloc)
	defer stop()
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
		until := time.Now().Add(5 * time.Second)
		for !eval(js) {
			if time.Now().After(until) {
				t.Fatal("UI timeout", js)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 844), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && typeof openReplyContext==='function'`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated&&!state.postsLoading&&!!document.querySelector('[data-reply-context="124"]')`)
	if !eval(`document.querySelector('[data-post-id="124"] .reply-preview-text').textContent.includes('自回复') && document.querySelector('[data-post-id="200"] .reply-preview-text').textContent.includes('尚未归档') && !window.replyXSS`) {
		t.Fatal("reply previews incorrect")
	}
	screenshot := func(name string) {
		t.Helper()
		if dir := os.Getenv("XM_SCREENSHOT_DIR"); dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, name), shot, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	screenshot("self-replies-mobile.png")
	for _, theme := range []string{"light", "dark"} {
		eval(`(()=>{applyArchiveTheme('` + theme + `');document.querySelector('[data-reply-context="124"]').click();return true})()`)
		wait(`$('reply-context-dialog').open&&!replyContextLoading&&$('reply-context-items').children.length===20`)
		if !eval(`$('reply-context-items').firstElementChild.dataset.contextPost==='105' && $('reply-context-items').lastElementChild.dataset.contextPost==='124' && document.querySelectorAll('#reply-context-items [aria-current=true]').length===1 && !$('reply-context-more').hidden && !document.querySelector('#reply-context-items img') && !window.replyXSS`) {
			t.Fatal("context ordering/current/escaping incorrect")
		}
		if !eval(`(()=>{const d=$('reply-context-dialog'),r=d.getBoundingClientRect();return r.left>=0&&r.right<=innerWidth&&d.scrollWidth<=d.clientWidth+1})()`) {
			t.Fatal("mobile context overflow")
		}
		screenshot("reply-context-" + theme + "-mobile.png")
		eval(`(()=>{$('reply-context-more').click();return true})()`)
		wait(`!replyContextLoading&&$('reply-context-items').children.length===25`)
		if !eval(`$('reply-context-items').firstElementChild.dataset.contextPost==='100'&&$('reply-context-more').hidden`) {
			t.Fatal("earlier page not prepended")
		}
		eval(`(()=>{$('reply-context-dialog').close();return true})()`)
		wait(`!$('reply-context-dialog').open&&$('reply-context-items').children.length===0`)
	}
	eval(`(()=>{document.querySelector('[data-reply-context="200"]').click();return true})()`)
	wait(`!replyContextLoading&&$('reply-context-status').textContent.includes('尚未归档')`)
	eval(`(()=>{$('reply-context-dialog').close();return true})()`)
	wait(`$('reply-context-items').children.length===0`)
	// A delayed response cannot repopulate a dialog closed during navigation.
	eval(`(()=>{window.savedReplyAPI=api;api=()=>new Promise(resolve=>window.finishReplyRequest=resolve);openReplyContext('124');return true})()`)
	eval(`(()=>{closeReplyContext();return true})()`)
	wait(`!$('reply-context-dialog').open&&$('reply-context-items').children.length===0`)
	eval(`(()=>{window.finishReplyRequest({items:[{id:'124',username:'alice',text:'stale'}],end:'beginning'});api=window.savedReplyAPI;return true})()`)
	if !eval(`!$('reply-context-dialog').open&&$('reply-context-items').children.length===0`) {
		t.Fatal("stale context response applied")
	}
}
