package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestMediaLazyPlaybackAndBrowserCache(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	seedCachedMedia(t, a, "100", "photo", ".png", png)
	// Deliberately invalid data also exercises the retry UI after release.
	seedCachedMedia(t, a, "200", "video", ".mp4", []byte("offline synthetic video fixture"))
	var images, videos atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	handler := a.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/media/100/") {
			images.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/media/200/") {
			videos.Add(1)
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
				t.Fatal("UI timed out:", js)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(1280, 900), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated && !state.postsLoading && state.postIDs.size===2 && document.querySelector('.media-button img')?.naturalWidth>0`)
	if videos.Load() != 0 || !eval(`document.querySelector('video').getAttribute('src')===null && document.querySelector('video').hidden && !document.querySelector('.video-load').hidden`) {
		t.Fatal("video fetched before a play action", videos.Load())
	}
	for _, width := range []int64{1280, 390} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
			t.Fatal(err)
		}
		for _, theme := range []string{"light", "dark"} {
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`(()=>{const r=document.querySelector('.video-load').getBoundingClientRect();return Math.abs(r.width/r.height-16/9)<.05 && document.documentElement.scrollWidth<=innerWidth})()`) {
				t.Fatalf("placeholder size/overflow at %d %s", width, theme)
			}
		}
	}
	imageRequests := images.Load()
	eval(`(()=>{location.hash='archive';return true})()`)
	wait(`state.view==='archive'`)
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && !state.postsLoading && document.querySelector('.media-button img')?.naturalWidth>0`)
	if images.Load() != imageRequests || videos.Load() != 0 {
		t.Fatal("browser did not reuse original image or eagerly loaded video", imageRequests, images.Load(), videos.Load())
	}
	eval(`(()=>{document.querySelector('.video-load').click();return true})()`)
	wait(`document.querySelector('video').getAttribute('src')?.includes('?v=') && document.querySelector('video').muted && !document.querySelector('video').hidden`)
	deadline := time.Now().Add(5 * time.Second)
	for videos.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if videos.Load() == 0 {
		t.Fatal("play did not request the video")
	}
	eval(`(()=>{window.testPlayingVideo=document.querySelector('video');location.hash='settings';return true})()`)
	wait(`state.view==='settings' && window.testPlayingVideo.getAttribute('src')===null && window.testPlayingVideo.paused`)
	eval(`(()=>{$('settings-tab-display').click();return true})()`)
	wait(`!$('settings-display').hidden && $('image-cache-hours').value==='168'`)
	eval(`(()=>{$('video-default-muted').checked=false;$('image-cache-hours').value='48';$('video-cache-minutes').value='5';$('archive-display-form').requestSubmit();return true})()`)
	wait(`state.settings.video_default_muted===false && state.settings.image_cache_seconds===172800 && state.settings.video_cache_seconds===300`)
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && !state.postsLoading && !!document.querySelector('.video-load')`)
	eval(`(()=>{document.querySelector('.video-load').click();return true})()`)
	if !eval(`document.querySelector('video').muted===false && document.querySelector('video').getAttribute('src')!==null`) {
		t.Fatal("saved mute preference not applied")
	}
	// Force loading even if Chrome requires an extra gesture for unmuted play.
	eval(`(()=>{document.querySelector('video').load();return true})()`)
	unblock()
	wait(`!document.querySelector('.video-load').hidden && document.querySelector('.video-load strong').textContent==='重试播放'`)
	if !eval(`document.querySelector('video').getAttribute('src')===null`) {
		t.Fatal("failed video kept loading")
	}
}
