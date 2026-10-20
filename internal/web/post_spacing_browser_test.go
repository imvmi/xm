package web

import (
	"context"
	"encoding/base64"
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

func TestPostContentSpacingBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	seedCachedMedia(t, a, "100", "photo", ".png", png)
	seedCachedMedia(t, a, "200", "video", ".mp4", []byte("not requested until play"))
	for id, text := range map[string]string{"100": "\n https://t.co/photo\n", "200": "这是正文，下面是视频。\n\n https://t.co/video"} {
		if _, err := a.store.DB.Exec("UPDATE posts SET text=? WHERE id=?", text, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "300", Username: "alice", Text: "第一行\n第二行\n\n下一段"}, {ID: "400", Username: "alice", Text: "带有图片引用的正文", QuoteID: "100"}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	alloc, stopAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer stopAlloc()
	browser, stopBrowser := chromedp.NewContext(alloc)
	defer stopBrowser()
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
		deadline := time.Now().Add(5 * time.Second)
		for !eval(js) {
			if time.Now().After(deadline) {
				t.Fatal("UI timed out", js)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined'`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated && !state.postsLoading && state.postIDs.size===4`)
	for _, width := range []int64{1280, 390} {
		opts := []chromedp.EmulateViewportOption{}
		if width == 390 {
			opts = append(opts, chromedp.EmulateTouch, chromedp.EmulateMobile)
		}
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844, opts...)); err != nil {
			t.Fatal(err)
		}
		for _, theme := range []string{"light", "dark"} {
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			for _, view := range []string{"home", "user/alice"} {
				eval(fmt.Sprintf(`(()=>{location.hash=%q;return true})()`, view))
				name := "home"
				if view != "home" {
					name = "user"
				}
				wait(fmt.Sprintf(`state.view===%q && !state.postsLoading && state.postIDs.size===4 && !!document.querySelector('article[data-post-id="400"] .quote-card')`, name))
				if !eval(`(()=>{
 const post=id=>document.querySelector('article[data-post-id="'+id+'"]');
 const photo=post('100'),video=post('200'),paragraphs=post('300');
 const header=p=>p.querySelector('.post-header').getBoundingClientRect();
 const media=p=>p.querySelector('.media-grid').getBoundingClientRect();
 const text=p=>p.querySelector('.post-main>.post-text').getBoundingClientRect();
 const quote=post('400').querySelector('.quote-card');
 const touch=matchMedia('(pointer:coarse)').matches;
 return header(photo).height<=23 && header(video).height<=23 &&
  media(photo).top-header(photo).bottom<=6.1 && media(photo).top>=header(photo).bottom && text(photo).height===0 &&
  text(video).top-header(video).bottom<=2.1 && media(video).top-text(video).bottom<=6.1 &&
  video.querySelector('.post-text').textContent==='这是正文，下面是视频。' &&
  paragraphs.querySelector('.post-text').textContent==='第一行\n第二行\n\n下一段' &&
  quote.querySelector('.post-text').getBoundingClientRect().height===0 &&
  quote.querySelector('.media-grid').getBoundingClientRect().top-quote.querySelector('.quote-heading').getBoundingClientRect().bottom<=6.1 &&
  [...document.querySelectorAll('#posts .bookmark-button')].every(b=>{const r=b.getBoundingClientRect();return r.width>=(touch?44:40)&&r.height>=(touch?44:40)}) &&
  document.documentElement.scrollWidth<=innerWidth;
})()`) {
					detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify([...document.querySelectorAll('#posts article')].map(p=>({id:p.dataset.postId,h:p.querySelector('.post-header').getBoundingClientRect().height,gap:p.querySelector('.media-grid')?.getBoundingClientRect().top-p.querySelector('.post-header').getBoundingClientRect().bottom,text:p.querySelector('.post-text').textContent})))`))
					t.Fatalf("spacing regression %d %s %s: %s", width, theme, view, detail)
				}
			}
		}
		if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
			eval(`(()=>{document.querySelector('article[data-post-id="200"]').scrollIntoView({block:'start'});return true})()`)
			shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("post-spacing-%d.png", width)), shot, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
