package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestGalleryCenteredSwipeBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	assets := []archive.Asset{{ID: "portrait", Kind: "photo"}, {ID: "landscape", Kind: "photo"}, {ID: "square", Kind: "photo"}}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", Username: "alice", Text: "图片居中与滑动测试\n第二行正文", Assets: assets}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.Config().DataDir, "media")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	for i, size := range []image.Point{{X: 400, Y: 800}, {X: 1200, Y: 500}, {X: 700, Y: 700}} {
		img := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
		for y := 0; y < size.Y; y++ {
			for x := 0; x < size.X; x++ {
				img.SetRGBA(x, y, color.RGBA{R: uint8(40 + x*120/size.X), G: uint8(70 + y*100/size.Y), B: uint8(110 + i*35), A: 255})
			}
		}
		var data bytes.Buffer
		if err := png.Encode(&data, img); err != nil {
			t.Fatal(err)
		}
		name := assets[i].ID + ".png"
		if err := os.WriteFile(filepath.Join(dir, name), data.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.store.MediaResult(archive.MediaJob{PostID: "100", ID: assets[i].ID, StorageID: "local"}, "media/"+name, int64(data.Len()), fmt.Sprintf("%x", sha256.Sum256(data.Bytes())), nil); err != nil {
			t.Fatal(err)
		}
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
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({index:imageIndex,busy:galleryBusy,drag:galleryDrag,width:innerWidth,src:$('large-image').src,loaded:$('large-image').naturalWidth,open:$('image-dialog').open,count:$('image-count').textContent})`))
				t.Fatal("UI timed out", js, detail)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	password, _ := json.Marshal(testPassword)
	wait(`typeof openGallery==='function'`)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated && !state.postsLoading && document.querySelectorAll('[data-image]').length===3`)
	for _, width := range []int64{1280, 390} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
			t.Fatal(err)
		}
		for _, theme := range []string{"light", "dark"} {
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);document.querySelector('[data-image]').click();return true})()`, theme))
			for i := 0; i < 3; i++ {
				wait(fmt.Sprintf(`!galleryBusy && imageIndex===%d && $('large-image').complete && $('large-image').naturalWidth>0`, i))
				if !eval(`(()=>{const r=$('large-image').getBoundingClientRect();return Math.abs(r.x+r.width/2-innerWidth/2)<2 && Math.abs(r.y+r.height/2-innerHeight/2)<2 && r.width<=innerWidth && r.height<=innerHeight && document.documentElement.scrollWidth<=innerWidth})()`) {
					t.Fatalf("image not centered at %d %s image %d", width, theme, i)
				}
				if i < 2 {
					eval(`(()=>{$('image-dialog').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowRight'}));return true})()`)
				}
			}
			// The final slide stays put instead of wrapping around.
			eval(`(()=>{stepGallery(1);return true})()`)
			wait(`!galleryBusy && imageIndex===2`)
			eval(`(()=>{$('image-dialog').dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowLeft'}));return true})()`)
			wait(`!galleryBusy && imageIndex===1`)
			if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
				shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("gallery-%s-%d.png", theme, width)), shot, 0600); err != nil {
					t.Fatal(err)
				}
			}
			eval(`(()=>{$('image-dialog').close();return true})()`)
			wait(`imageGallery.length===0 && !$('large-image').getAttribute('src') && !$('gallery-before').getAttribute('src') && !$('gallery-after').getAttribute('src') && document.body.style.overflow===''`)
		}
	}
	// Real touch input: drag follows the finger, then settles on release.
	touch := func(kind input.DispatchTouchEventType, x float64, y ...float64) {
		t.Helper()
		err := chromedp.Do(browser, chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
			points := []*input.TouchPoint{}
			if kind != input.DispatchTouchEventTypeTouchEnd && kind != input.DispatchTouchEventTypeTouchCancel {
				posY := 450.0
				if len(y) > 0 {
					posY = y[0]
				}
				points = append(points, &input.TouchPoint{X: x, Y: posY})
			}
			_, err := cdp.Call(ctx, target, input.DispatchTouchEvent, input.DispatchTouchEventParams{Type: kind, TouchPoints: points})
			return err
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	eval(`(()=>{$('posts').style.paddingBottom='1200px';window.scrollTo(0,120);document.querySelector('[data-image]').click();return true})()`)
	wait(`$('image-dialog').open && imageIndex===0`)
	if !eval(`!$('image-dialog').querySelector('button') && document.activeElement===$('image-dialog')`) {
		t.Fatal("gallery has visible buttons or did not take keyboard focus")
	}
	touch(input.DispatchTouchEventTypeTouchStart, 280)
	touch(input.DispatchTouchEventTypeTouchMove, 170)
	wait(`galleryDrag?.axis==='horizontal' && Math.abs(galleryDrag.dx+110)<2 && imageIndex===0`)
	touch(input.DispatchTouchEventTypeTouchEnd, 170)
	wait(`!galleryBusy && imageIndex===1`)
	touch(input.DispatchTouchEventTypeTouchStart, 200)
	touch(input.DispatchTouchEventTypeTouchMove, 190)
	touch(input.DispatchTouchEventTypeTouchEnd, 190)
	wait(`!galleryBusy && imageIndex===1`)
	touch(input.DispatchTouchEventTypeTouchStart, 280)
	touch(input.DispatchTouchEventTypeTouchMove, 170)
	touch(input.DispatchTouchEventTypeTouchCancel, 170)
	wait(`!galleryBusy && imageIndex===1`)
	eval(`(()=>{showGalleryImage(0);return true})()`)
	touch(input.DispatchTouchEventTypeTouchStart, 180)
	touch(input.DispatchTouchEventTypeTouchMove, 280)
	wait(`galleryDrag?.axis==='horizontal'`)
	if !eval(`galleryTrack.style.transform.includes('25px')`) {
		t.Fatal("first-slide overscroll did not resist dragging")
	}
	touch(input.DispatchTouchEventTypeTouchEnd, 280)
	wait(`!galleryBusy && imageIndex===0`)
	// Vertical gestures follow the finger. Small drags spring back; upward and
	// downward swipes close without navigating or changing the timeline position.
	eval(`(()=>{window.galleryScroll=scrollY;window.galleryHash=location.hash;return true})()`)
	if !eval(`window.galleryScroll>0`) {
		t.Fatal("scroll restoration fixture did not scroll")
	}
	touch(input.DispatchTouchEventTypeTouchStart, 180, 350)
	touch(input.DispatchTouchEventTypeTouchMove, 180, 375)
	wait(`galleryDrag?.axis==='vertical' && galleryViewport.style.transform.includes('25px')`)
	touch(input.DispatchTouchEventTypeTouchEnd, 180)
	wait(`!galleryBusy && $('image-dialog').open`)
	touch(input.DispatchTouchEventTypeTouchStart, 180, 450)
	touch(input.DispatchTouchEventTypeTouchMove, 180, 425)
	touch(input.DispatchTouchEventTypeTouchEnd, 180)
	wait(`!galleryBusy && $('image-dialog').open`)
	touch(input.DispatchTouchEventTypeTouchStart, 180, 450)
	touch(input.DispatchTouchEventTypeTouchMove, 180, 250)
	wait(`galleryDrag?.axis==='vertical' && galleryViewport.style.transform.includes('-200px')`)
	touch(input.DispatchTouchEventTypeTouchEnd, 180)
	wait(`!$('image-dialog').open && imageGallery.length===0 && document.body.style.overflow==='' && scrollY===window.galleryScroll && location.hash===window.galleryHash`)
	eval(`(()=>{document.querySelector('[data-image]').click();return true})()`)
	wait(`$('image-dialog').open && imageIndex===0`)
	touch(input.DispatchTouchEventTypeTouchStart, 180, 350)
	touch(input.DispatchTouchEventTypeTouchMove, 180, 550)
	touch(input.DispatchTouchEventTypeTouchEnd, 180)
	wait(`!$('image-dialog').open && imageGallery.length===0 && document.body.style.overflow==='' && scrollY===window.galleryScroll && location.hash===window.galleryHash`)
	// Single-image posts support dismissing too, despite having no next slide.
	eval(`(()=>{openGallery([document.querySelector('[data-image]').dataset.image]);return true})()`)
	touch(input.DispatchTouchEventTypeTouchStart, 180, 350)
	touch(input.DispatchTouchEventTypeTouchMove, 180, 550)
	touch(input.DispatchTouchEventTypeTouchEnd, 180)
	wait(`!$('image-dialog').open && imageGallery.length===0`)
	eval(`(()=>{document.querySelector('[data-image]').click();return true})()`)
	wait(`$('image-dialog').open && imageIndex===0`)
	// Resizing while sliding retains the target; closing clears pending callbacks.
	if !eval(`(()=>{stepGallery(1);window.dispatchEvent(new Event('resize'));return imageIndex===1&&!galleryBusy})()`) {
		t.Fatal("resize lost pending slide")
	}
	eval(`(()=>{stepGallery(1);$('image-dialog').close();return true})()`)
	wait(`imageGallery.length===0 && galleryTimer===null && !galleryBusy`)
	eval(`(()=>{document.querySelector('[data-image]').click();return true})()`)
	wait(`imageIndex===0 && $('image-dialog').open`)
	// Mouse dragging supports the desktop viewer as well.
	eval(`(()=>{window.galleryResized=false;window.addEventListener('resize',()=>window.galleryResized=true,{once:true});return true})()`)
	if err := chromedp.Do(browser, chromedp.EmulateViewport(1280, 900)); err != nil {
		t.Fatal(err)
	}
	wait(`window.galleryResized && galleryViewport.clientWidth===1280`)
	if err := chromedp.Do(browser, chromedp.MouseEvent(chromedp.MousePressed, 640, 450, chromedp.ButtonLeft), chromedp.MouseEvent(chromedp.MouseMoved, 480, 450, chromedp.ButtonLeft)); err != nil {
		t.Fatal(err)
	}
	wait(`galleryDrag?.axis==='horizontal'`)
	if err := chromedp.Do(browser, chromedp.MouseEvent(chromedp.MouseReleased, 480, 450, chromedp.ButtonLeft)); err != nil {
		t.Fatal(err)
	}
	wait(`!galleryBusy && imageIndex===1`)
	// Reduced-motion preference switches immediately.
	if err := chromedp.Do(browser, chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
		_, err := cdp.Call(ctx, target, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{Features: []*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}}})
		return err
	})); err != nil {
		t.Fatal(err)
	}
	if !eval(`(()=>{stepGallery(1);return imageIndex===2&&!galleryBusy})()`) {
		t.Fatal("reduced motion ignored")
	}
	eval(`(()=>{$('image-dialog').close();return true})()`)
	wait(`imageGallery.length===0`)
	// Cross-post navigation follows the selected timeline and fetches through
	// text-only pages. Start from a non-first image to check both directions.
	payload, err := os.ReadFile(filepath.Join(dir, "portrait.png"))
	if err != nil {
		t.Fatal(err)
	}
	seedCachedMedia(t, a, "50", "photo", ".png", payload)
	seedCachedMedia(t, a, "49", "photo", ".png", payload)
	for i := 0; i < 22; i++ {
		if err := a.store.SavePosts("alice", []archive.Post{{ID: fmt.Sprint(60 + i), Username: "alice", Text: "无图片的推文"}}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := a.Config()
	cfg.TimelinePageSize = 10
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	eval(`(()=>{const sheet=document.styleSheets[0];sheet.insertRule('#posts>.post{min-height:400px}',sheet.cssRules.length);refreshPosts();return true})()`)
	wait(`!state.postsLoading && state.postIDs.size===10`)
	eval(`(()=>{document.querySelectorAll('[data-image]')[2].click();stepGallery(1);return true})()`)
	wait(`!galleryLoading && !galleryBusy && imageIndex===3 && $('large-image').src.includes('/media/50/') && state.postIDs.size===25`)
	eval(`(()=>{stepGallery(1);return true})()`)
	wait(`imageIndex===4 && $('large-image').src.includes('/media/49/')`)
	eval(`(()=>{stepGallery(-1);stepGallery(-1);return true})()`)
	wait(`imageIndex===2 && $('large-image').src.includes('/media/100/')`)
	eval(`(()=>{$('image-dialog').close();return true})()`)
	wait(`imageGallery.length===0`)
	eval(`(()=>{document.querySelector('[data-image^="/media/50/"]').click();return true})()`)
	wait(`imageIndex===3 && imageGallery.length===5`)
	eval(`(()=>{location.hash='user/alice';return true})()`)
	wait(`!$('image-dialog').open && state.view==='user' && !state.postsLoading && document.body.style.overflow===''`)

}
