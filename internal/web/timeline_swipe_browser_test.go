package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestTimelineSwipeBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome timeline swipe fixture")
	}
	a := testApp(t)
	if err := a.store.SavePosts("alice", []archive.Post{
		{ID: "100", Username: "alice", Text: "收藏的推文"},
		{ID: "101", Username: "alice", Text: "另一条推文"},
		{ID: "102", Username: "alice", Text: "回复", ReplyTo: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO post_bookmarks(post_id,created_at) VALUES('100',123)"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		deadline := time.Now().Add(7 * time.Second)
		for !eval(js) {
			if time.Now().After(deadline) {
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({tab:state.timelineTab,view:state.view,loading:state.postsLoading,hash:location.hash,inert:$('timeline-panel').inert,transform:getComputedStyle($('timeline-panel')).transform,posts:[...state.postIDs],events:window.swipeEvents?.slice(-8)})`))
				t.Fatal("timeline swipe timeout", js, detail)
			}
			time.Sleep(20 * time.Millisecond)
		}
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
	swipe := func(from, to float64, cancelled bool) {
		t.Helper()
		y, err := chromedp.Run(browser, chromedp.Evaluate[float64](fmt.Sprintf(`(()=>{const p=$('timeline-panel'),r=p.getBoundingClientRect();for(let y=Math.max(250,r.top+120);y<Math.min(innerHeight-100,r.bottom-20);y+=20){const el=document.elementFromPoint(%f,y);if(p.contains(el)&&!el.closest('a,button,input,select,video,.video-slot'))return y;}return -1;})()`, from)))
		if err != nil || y < 0 {
			t.Fatal("no visible timeline swipe surface", y, err)
		}
		touch(input.DispatchTouchEventTypeTouchStart, from, y)
		touch(input.DispatchTouchEventTypeTouchMove, (from+to)/2, y)
		touch(input.DispatchTouchEventTypeTouchMove, to, y)
		if cancelled {
			touch(input.DispatchTouchEventTypeTouchCancel, to, y)
		} else {
			touch(input.DispatchTouchEventTypeTouchEnd, to, y)
		}
	}
	settled := `!state.postsLoading&&!$('timeline-panel').inert&&!document.querySelector('.timeline-sliding')`
	assertTab := func(tab string, count int) {
		t.Helper()
		wait(fmt.Sprintf(`state.timelineTab===%q&&state.postIDs.size===%d&&`+settled, tab, count))
		if !eval(`!state.focusPost&&getComputedStyle($('timeline-panel')).transform==='none'&&$('timeline-panel').getAnimations().length===0&&document.documentElement.scrollWidth<=innerWidth&&$( $('timeline-panel').getAttribute('aria-labelledby')).getAttribute('aria-selected')==='true'`) {
			t.Fatal("swipe left stale transforms, overflow, incorrect accessibility or opened a post")
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 844), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof animateTimelineTab==='function'`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	assertTab("", 2)
	eval(`(()=>{window.swipeEvents=[];for(const type of ['pointerdown','pointermove','pointerup','pointercancel','lostpointercapture'])document.addEventListener(type,e=>swipeEvents.push({type:e.type,target:e.target.outerHTML.slice(0,160),x:e.clientX,y:e.clientY,primary:e.isPrimary}),true);window.addEventListener('resize',()=>swipeEvents.push({type:'resize'}));return true})()`)
	// A real touch drag tracks the finger and clips horizontally, then advances
	// to the empty following feed. Empty feeds must also accept the next swipe.
	touch(input.DispatchTouchEventTypeTouchStart, 320, 420)
	touch(input.DispatchTouchEventTypeTouchMove, 170, 420)
	if !eval(`$('home-feed').classList.contains('timeline-dragging')&&new DOMMatrix(getComputedStyle($('timeline-panel')).transform).m41<-100&&document.documentElement.scrollWidth<=innerWidth`) {
		detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({events:swipeEvents,feed:$('home-feed').className,panel:$('timeline-panel').getBoundingClientRect().toJSON(),transform:getComputedStyle($('timeline-panel')).transform,width:document.documentElement.scrollWidth,reduced:matchMedia('(prefers-reduced-motion: reduce)').matches})`))
		t.Fatal("timeline did not follow the finger or escaped its viewport", detail)
	}
	touch(input.DispatchTouchEventTypeTouchEnd, 170, 420)
	wait(`$('timeline-panel').getAnimations().length>0`)
	assertTab("following", 0)
	swipe(320, 70, false)
	assertTab("bookmarks", 1)
	swipe(320, 70, false) // Last tab resists; it never wraps.
	assertTab("bookmarks", 1)
	swipe(70, 320, false)
	assertTab("following", 0)
	swipe(70, 320, false)
	assertTab("", 2)
	swipe(70, 320, false) // First tab also resists.
	assertTab("", 2)
	swipe(240, 220, false) // Short drag springs back.
	assertTab("", 2)
	swipe(320, 70, true) // Cancelled touch must not commit.
	assertTab("", 2)
	// Vertical gestures scroll without changing the selected tab.
	eval(`(()=>{$('posts').style.paddingBottom='1200px';return true})()`)
	touch(input.DispatchTouchEventTypeTouchStart, 200, 520)
	touch(input.DispatchTouchEventTypeTouchMove, 205, 300)
	touch(input.DispatchTouchEventTypeTouchEnd, 205, 300)
	wait(`scrollY>50`)
	assertTab("", 2)
	eval(`(()=>{$('posts').style.removeProperty('padding-bottom');window.scrollTo({top:0,behavior:'instant'});return true})()`)
	// An outgoing transition may be superseded by another click or a route.
	eval(`(()=>{$('home-tab-following').click();$('home-tab-bookmarks').click();return true})()`)
	assertTab("bookmarks", 1)
	eval(`(()=>{$('home-tab-all').click();location.hash='user/alice';return true})()`)
	wait(`state.view==='user'&&state.target==='alice'&&` + settled)
	assertTab("posts", 2)
	eval(`(()=>{window.scrollTo({top:$('timeline-panel').getBoundingClientRect().top+scrollY-180,behavior:'instant'});return true})()`)
	for _, item := range []struct {
		tab   string
		count int
	}{{"replies", 1}, {"media", 0}, {"bookmarks", 1}} {
		swipe(320, 70, false)
		assertTab(item.tab, item.count)
	}
	swipe(70, 320, false)
	assertTab("media", 0)
	eval(`(()=>{$('tab-posts').click();return true})()`)
	assertTab("posts", 2)
	// Keyboard navigation shares the animated switch and keeps focus on tabs.
	eval(`(()=>{$('tab-posts').focus();$('tab-posts').dispatchEvent(new KeyboardEvent('keydown',{key:'End',bubbles:true}));return true})()`)
	assertTab("bookmarks", 1)
	if !eval(`document.activeElement===$('tab-bookmarks')`) {
		t.Fatal("tab keyboard focus was lost")
	}
	// Reduced motion still switches, without a transform or animation.
	if err := chromedp.Do(browser, chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
		_, err := cdp.Call(ctx, target, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{Features: []*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}}})
		return err
	})); err != nil {
		t.Fatal(err)
	}
	wait(`matchMedia('(prefers-reduced-motion: reduce)').matches`)
	swipe(70, 320, false)
	assertTab("media", 0)
	if !eval(`getComputedStyle(document.querySelector('#view-user .timeline-tabs'),'::before').transitionDuration==='0s'`) {
		t.Fatal("tab indicator ignored reduced motion")
	}
}
