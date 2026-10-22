package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestGalleryPrefetchBeforePageBoundary(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	cfg := a.Config()
	cfg.TimelinePageSize = 10
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	for i := 0; i < 20; i++ {
		seedCachedMedia(t, a, strconv.Itoa(100+i), "photo", ".png", png)
	}
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var pages atomic.Int32
	handler := a.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/posts" && r.URL.Query().Get("before") != "" {
			pages.Add(1)
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
		until := time.Now().Add(8 * time.Second)
		for !eval(js) {
			if time.Now().After(until) {
				snapshot, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({view:state.view,loading:state.postsLoading,count:state.postIDs.size,pageSize:state.settings?.timeline_page_size,error:state.postsError,status:$("load-status").textContent,hash:location.hash})`))
				t.Fatal("prefetch UI timed out", js, snapshot)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 844), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && typeof requestGalleryPrefetch==='function'`)
	eval(`(()=>{document.styleSheets[0].insertRule('#posts>.post{min-height:700px}',document.styleSheets[0].cssRules.length);return true})()`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated&&!state.postsLoading&&state.postIDs.size===10`)
	if pages.Load() != 0 {
		t.Fatal("fixture loaded the next page before opening gallery")
	}
	eval(`(()=>{document.querySelectorAll('[data-image]')[7].click();return true})()`)
	wait(`$('image-dialog').open && imageIndex===7 && state.postsLoading`)
	if pages.Load() > 1 {
		t.Fatal("duplicate speculative request")
	}
	// A slow speculative page must not block navigation through available images.
	eval(`(()=>{stepGallery(1);return true})()`)
	wait(`imageIndex===8 && !galleryBusy && !galleryLoading`)
	unblock()
	wait(`!state.postsLoading && state.postIDs.size===20 && imageGallery.length===20`)
	if pages.Load() != 1 {
		t.Fatal("next page not shared", pages.Load())
	}
	if !eval(`imageIndex===8 && $('gallery-after').getAttribute('src')===imageGallery[9]`) {
		t.Fatal("prefetch changed selection or failed to refresh neighbor")
	}
	eval(`(()=>{showGalleryImage(9);return true})()`)
	wait(`$('large-image').complete && galleryPrefetchImages.size===2`)
	eval(`(()=>{stepGallery(1);return true})()`)
	wait(`imageIndex===10 && !galleryBusy && !galleryLoading`)
	// The actual settings control persists, and disabling speculation takes effect.
	eval(`(()=>{$('image-dialog').close();location.hash='settings';return true})()`)
	wait(`state.view==='settings' && settingsBaselines.has('archive-display-form') && $('timeline-page-size').value==='10'`)
	eval(`(()=>{$('settings-tab-display').click();$('media-preload-mode').value='off';$('archive-display-form').requestSubmit();return true})()`)
	wait(`state.settings.media_preload_mode==='off' && $('archive-display-result').textContent.includes('已保存')`)
	loaded, err := a.store.LoadSettings(a.Config())
	if err != nil || loaded.MediaPreloadMode != "off" {
		t.Fatal("preload control did not persist", err)
	}
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home'&&!state.postsLoading&&state.postIDs.size===10`)
	before := pages.Load()
	eval(`(()=>{document.querySelectorAll('[data-image]')[7].click();window.prefetchDelayDone=false;setTimeout(()=>window.prefetchDelayDone=true,150);return true})()`)
	wait(`window.prefetchDelayDone`)
	if pages.Load() != before || !eval(`imageIndex===7 && state.postIDs.size===10 && galleryPrefetchImages.size===0`) {
		t.Fatal("disabled mode still speculated")
	}
	eval(`(()=>{$('image-dialog').close();return true})()`)
	if !eval(`galleryPrefetchImages.size===0 && !prefetchedVideo`) {
		t.Fatal("close leaked speculative media")
	}
}
