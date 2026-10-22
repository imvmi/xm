package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestReplyThreadPageAndSwipeBackBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome reply-thread fixture")
	}
	a := testApp(t)
	if err := a.store.SavePosts("alice", []archive.Post{
		{ID: "100", AuthorID: "42", Username: "alice", Text: "根推文：回复只应作为附属信息。", CreatedAt: "2026-10-22T08:00:00Z"},
		{ID: "101", AuthorID: "42", Username: "alice", ReplyTo: "100", ReplyToAuthorID: "42", Text: "Alice 的直接回复", CreatedAt: "2026-10-22T08:10:00Z"},
		{ID: "400", AuthorID: "42", Username: "alice", Text: "另一条根推文", CreatedAt: "2026-10-22T09:00:00Z"},
		{ID: "500", AuthorID: "42", Username: "alice", ReplyTo: "999", ReplyToAuthorID: "42", Text: "父推文缺失的回复", CreatedAt: "2026-10-22T09:10:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("bob", []archive.Post{
		{ID: "200", AuthorID: "88", Username: "bob", ReplyTo: "100", ReplyToAuthorID: "42", Text: "Bob 的直接回复", CreatedAt: "2026-10-22T08:20:00Z"},
		{ID: "201", AuthorID: "99", Username: "carol", ReplyTo: "200", ReplyToAuthorID: "88", Text: `<img src=x onerror="window.replyXSS=true"> Carol 的下一层回复`, CreatedAt: "2026-10-22T08:30:00Z"},
	}); err != nil {
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
		v, err := chromedp.Run(browser, chromedp.Evaluate[bool]("Boolean("+js+")"))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	wait := func(js string) {
		t.Helper()
		until := time.Now().Add(7 * time.Second)
		for !eval(js) {
			if time.Now().After(until) {
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({hash:location.hash,focus:state.focusPost,loading:state.postsLoading,context:threadContextLoading,replies:threadRepliesLoading,posts:[...document.querySelectorAll('#posts article')].map(p=>p.dataset.postId),status:$('thread-replies-status')?.textContent})`))
				t.Fatal("UI timeout", js, detail)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 844), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && typeof openPostDetail==='function'`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated&&!state.postsLoading&&state.postIDs.size===2`)
	if !eval(`(()=>{const ids=[...document.querySelectorAll('#posts>article')].map(p=>p.dataset.postId);return ids.join(',')==='400,100'&&!document.body.textContent.includes('Alice 的直接回复')&&document.querySelector('[data-post-id="100"] [data-open-post="100"]').textContent.includes('查看回复')})()`) {
		t.Fatal("replies leaked into the home timeline or reply affordance is missing")
	}
	eval(`(()=>{$('posts').style.paddingBottom='1200px';window.scrollTo(0,120);window.homeThreadScroll=scrollY;document.querySelector('[data-post-id="100"] .post-text').click();return true})()`)
	wait(`state.focusPost==='100'&&!threadContextLoading&&!threadRepliesLoading&&threadReplySeen.size===2`)
	if !eval(`location.hash==='#post/100'&&$('home-feed-title').textContent==='推文详情'&&document.querySelector('.thread-current').dataset.postId==='100'&&!!document.querySelector('.thread-reply[data-post-id="101"]')&&!!document.querySelector('.thread-reply[data-post-id="200"]')&&!document.querySelector('[data-post-id="201"]')&&!window.replyXSS`) {
		t.Fatal("root thread page did not show only its direct archived replies")
	}
	if !eval(`document.querySelector('.thread-reply[data-post-id="200"] [data-open-post="200"]').textContent.includes('查看回复')`) {
		t.Fatal("nested reply affordance missing")
	}
	eval(`(()=>{window.scrollTo(0,80);document.querySelector('.thread-reply[data-post-id="200"] .post-text').click();return true})()`)
	wait(`state.focusPost==='200'&&!threadContextLoading&&!threadRepliesLoading&&threadReplySeen.has('201')`)
	if !eval(`location.hash==='#post/200'&&document.querySelector('.thread-ancestor').dataset.postId==='100'&&document.querySelector('.thread-current').dataset.postId==='200'&&document.querySelector('.thread-reply').dataset.postId==='201'&&!window.replyXSS`) {
		t.Fatal("nested thread page lost its ancestor, current post, child, or escaping")
	}
	touch := func(kind input.DispatchTouchEventType, x, y float64) {
		t.Helper()
		if err := chromedp.Do(browser, chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
			points := []*input.TouchPoint{}
			if kind != input.DispatchTouchEventTypeTouchEnd && kind != input.DispatchTouchEventTypeTouchCancel {
				points = append(points, &input.TouchPoint{X: x, Y: y})
			}
			_, err := cdp.Call(ctx, target, input.DispatchTouchEvent, input.DispatchTouchEventParams{Type: kind, TouchPoints: points})
			return err
		})); err != nil {
			t.Fatal(err)
		}
	}
	touch(input.DispatchTouchEventTypeTouchStart, 45, 500)
	touch(input.DispatchTouchEventTypeTouchMove, 300, 500)
	wait(`detailSwipe?.axis==='horizontal'&&detailSwipe.dx>200`)
	touch(input.DispatchTouchEventTypeTouchEnd, 300, 500)
	wait(`state.focusPost==='100'&&!threadContextLoading&&!threadRepliesLoading`)
	if !eval(`location.hash==='#post/100'`) {
		t.Fatal("first right swipe did not return to the parent thread page")
	}
	touch(input.DispatchTouchEventTypeTouchStart, 45, 500)
	touch(input.DispatchTouchEventTypeTouchMove, 300, 500)
	touch(input.DispatchTouchEventTypeTouchEnd, 300, 500)
	wait(`!state.focusPost&&!state.postsLoading&&state.view==='home'`)
	if !eval(`Math.abs(scrollY-window.homeThreadScroll)<2&&[...document.querySelectorAll('#posts>article')].map(p=>p.dataset.postId).join(',')==='400,100'`) {
		t.Fatal("right swipe did not restore the previous timeline and scroll position")
	}
	eval(`(()=>{location.hash='user/alice';return true})()`)
	wait(`state.view==='user'&&state.target==='alice'&&!state.postsLoading&&state.timelineTab==='posts'&&document.querySelectorAll('#posts>article').length===2`)
	if !eval(`[...document.querySelectorAll('#posts>article')].map(p=>p.dataset.postId).join(',')==='400,100'`) {
		t.Fatal("account posts tab still contains replies")
	}
	eval(`(()=>{$('tab-replies').click();return true})()`)
	wait(`state.timelineTab==='replies'&&!state.postsLoading&&!!document.querySelector('[data-post-id="101"]')`)
	if !eval(`!!document.querySelector('[data-post-id="101"]')&&!!document.querySelector('[data-post-id="500"]')`) {
		t.Fatal("account replies tab no longer exposes archived replies")
	}
}
