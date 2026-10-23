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

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestVideoFullscreenControlsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
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
		deadline := time.Now().Add(6 * time.Second)
		for !eval(js) {
			if time.Now().After(deadline) {
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({suppress:videoSuppressClick,fullscreen:!!document.fullscreenElement,expanded:!!expandedVideoShell,video:window.v&&{time:v.currentTime,duration:v.duration,paused:v.paused,ready:v.readyState,error:v.error?.message},recordError:window.recordError,videoError:window.videoError,hit:window.clickHit,load:document.querySelector('.video-load')?.textContent,hidden:document.querySelector('.video-load')?.hidden,source:document.querySelector('video')?.getAttribute('src')})`))
				t.Fatal("UI timed out:", js, detail)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	touchClicks := false
	swipe := func(down bool) {
		t.Helper()
		y := 220.0
		if !down {
			y = -220
		}
		if touchClicks {
			for _, kind := range []input.DispatchTouchEventType{input.DispatchTouchEventTypeTouchStart, input.DispatchTouchEventTypeTouchMove, input.DispatchTouchEventTypeTouchEnd} {
				if err := chromedp.Do(browser, chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
					points := []*input.TouchPoint{}
					if kind != input.DispatchTouchEventTypeTouchEnd {
						posY := 400.0
						if kind == input.DispatchTouchEventTypeTouchMove {
							posY += y
						}
						touchID, radius, force := 0.0, 1.0, 1.0
						points = append(points, &input.TouchPoint{X: 180, Y: posY, ID: &touchID, RadiusX: &radius, RadiusY: &radius, Force: &force})
					}
					_, err := cdp.Call(ctx, target, input.DispatchTouchEvent, input.DispatchTouchEventParams{Type: kind, TouchPoints: points})
					return err
				})); err != nil {
					t.Fatal(err)
				}
			}
			return
		}
		if err := chromedp.Do(browser, chromedp.MouseEvent(chromedp.MousePressed, 180, 400, chromedp.ButtonLeft), chromedp.MouseEvent(chromedp.MouseMoved, 180, 400+y, chromedp.ButtonLeft), chromedp.MouseEvent(chromedp.MouseReleased, 180, 400+y, chromedp.ButtonLeft)); err != nil {
			t.Fatal(err)
		}
	}
	click := func(selector string) {
		t.Helper()
		// Let the compositor receive resized/returned inline geometry before touch hit-testing.
		eval(`(()=>{window.videoFrameReady=false;requestAnimationFrame(()=>requestAnimationFrame(()=>window.videoFrameReady=true));return true})()`)
		wait(`window.videoFrameReady`)
		encoded, _ := json.Marshal(selector)
		point, err := chromedp.Run(browser, chromedp.Evaluate[[]float64](`(()=>{const r=document.querySelector(`+string(encoded)+`).getBoundingClientRect(),x=r.x+r.width/2,y=r.y+r.height/2;window.clickHit={x,y,element:document.elementFromPoint(x,y)?.outerHTML};return [x,y]})()`))
		if err != nil {
			t.Fatal(err)
		}
		action := chromedp.MouseClickXY(point[0], point[1])
		if touchClicks {
			action = chromedp.TapXY(point[0], point[1])
		}
		if err := chromedp.Do(browser, action); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(1280, 900), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof startTimelineVideo==='function'`)
	// Produce a tiny real moving video entirely offline. No ffmpeg, external
	// fixture downloads, application configuration, or live archive is needed.
	eval(`(()=>{
	 const canvas=document.createElement('canvas');canvas.width=320;canvas.height=180;
	 const context=canvas.getContext('2d'),stream=canvas.captureStream(12),chunks=[];
	 const recorder=new MediaRecorder(stream,{mimeType:'video/mp4'});
	 let frame=0;
	 const draw=()=>{context.fillStyle='#15304b';context.fillRect(0,0,320,180);context.fillStyle='#1d9bf0';context.fillRect(20+(frame++%12)*12,60,70,60);};
	 draw();const timer=setInterval(draw,80);
	 recorder.ondataavailable=e=>chunks.push(e.data);
	 recorder.onerror=e=>window.recordError=String(e.error);
	 recorder.onstop=()=>{clearInterval(timer);stream.getTracks().forEach(t=>t.stop());const reader=new FileReader();reader.onload=()=>window.recordedVideo=reader.result.split(',')[1];reader.readAsDataURL(new Blob(chunks,{type:'video/mp4'}));};
	 recorder.start();setTimeout(()=>recorder.stop(),1600);return true;
	})()`)
	wait(`!!window.recordedVideo`)
	encoded, err := chromedp.Run(browser, chromedp.Evaluate[string](`window.recordedVideo`))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	seedCachedMedia(t, a, "200", "video", ".mp4", payload)
	seedCachedMedia(t, a, "100", "video", ".mp4", payload)
	seedCachedMedia(t, a, "90", "video", ".mp4", payload)
	cfg := a.Config()
	cfg.TimelinePageSize = 10
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 22; i++ {
		if err := a.store.SavePosts("alice", []archive.Post{{ID: fmt.Sprint(150 + i), Username: "alice", Text: "没有视频的中间推文"}}); err != nil {
			t.Fatal(err)
		}
	}
	eval(`(()=>{const sheet=document.styleSheets[0];sheet.insertRule('#posts>.post{min-height:400px}',sheet.cssRules.length);return true})()`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated && !state.postsLoading && !!document.querySelector('.video-load')`)
	eval(`(()=>{document.querySelector('video').addEventListener('error',e=>window.videoError=e.target.error?.message);return true})()`)
	// A trusted click should request native fullscreen on desktop Chrome.
	click(".video-load")
	wait(`!!document.fullscreenElement && document.querySelector('video').readyState>=2`)
	eval(`(()=>{window.v=document.querySelector('video');window.originalSource=v.src;return true})()`)
	click(`[data-video-action="play"]`)
	wait(`v.paused && !!expandedVideoShell && !!document.fullscreenElement`)
	// Seek to the end to discover duration for streaming recorder output
	// before exercising our seek UI.
	eval(`(()=>{v.currentTime=1000000;return true})()`)
	wait(`Number.isFinite(v.duration) && v.duration>0 && !v.seeking`)
	eval(`(()=>{const seek=document.querySelector('.video-seek');seek.value=300;seek.dispatchEvent(new Event('input',{bubbles:true}));return true})()`)
	wait(`!v.seeking && Math.abs(v.currentTime/v.duration-.3)<.03`)
	eval(`(()=>{window.savedTime=v.currentTime;return true})()`)
	click("video")
	if !eval(`!!expandedVideoShell && !!document.fullscreenElement`) {
		t.Fatal("single click dismissed video")
	}
	swipe(true)
	wait(`!document.fullscreenElement && !expandedVideoShell`)
	if !eval(`v===document.querySelector('video') && v.src===window.originalSource && v.paused && Math.abs(v.currentTime-window.savedTime)<.03 && document.body.style.overflow===''`) {
		t.Fatal("fullscreen exit reset the video or left scrolling locked")
	}
	click("video")
	wait(`!!document.fullscreenElement && !!expandedVideoShell`)
	click(`[data-video-action="mute"]`)
	if !eval(`!v.muted && v.paused && !!document.fullscreenElement`) {
		t.Fatal("sound control toggled playback or fullscreen")
	}
	// Browser-initiated fullscreen exit (such as Escape) restores inline state.
	eval(`(()=>{document.exitFullscreen();return true})()`)
	wait(`!expandedVideoShell && !document.fullscreenElement && v.paused`)
	for _, width := range []int64{1280, 390} {
		touchClicks = width == 390
		opts := []chromedp.EmulateViewportOption{}
		if touchClicks {
			opts = append(opts, chromedp.EmulateTouch)
		}
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900, opts...)); err != nil {
			t.Fatal(err)
		}
		// Denied/unsupported native fullscreen must still support swipe-to-return.
		eval(`(()=>{v.closest('.video-shell').requestFullscreen=()=>Promise.reject(new Error('fixture: fullscreen denied'));return true})()`)
		for _, theme := range []string{"light", "dark"} {
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			click("video")
			wait(`!!expandedVideoShell && !document.fullscreenElement`)
			if !eval(`(()=>{const r=v.getBoundingClientRect(),controls=document.querySelector('.video-controls').getBoundingClientRect();return Math.abs(r.width-innerWidth)<2 && Math.abs(r.height-innerHeight)<2 && controls.left>=0 && controls.right<=innerWidth && v.currentTime===window.savedTime && v.paused && v.src===window.originalSource && getComputedStyle(document.querySelector('.video-seek')).backgroundColor==='rgba(0, 0, 0, 0)'})()`) {
				t.Fatalf("fallback layout/progress at %d %s", width, theme)
			}
			if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
				shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("video-fullscreen-%s-%d.png", theme, width)), shot, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if touchClicks {
				eval(`(()=>{window.savedMuted=v.muted;return true})()`)
				click(`[data-video-action="mute"]`)
				if !eval(`v.muted!==window.savedMuted && v.paused && !!expandedVideoShell`) {
					t.Fatal("touch control was missed or toggled twice")
				}
				click(`[data-video-action="mute"]`)
			}
			click("video")
			if !eval(`!!expandedVideoShell`) {
				t.Fatal("fallback single click dismissed video")
			}
			swipe(true)
			wait(`!expandedVideoShell`)
			if !eval(`(()=>{const r=v.getBoundingClientRect();return Math.abs(r.width/r.height-16/9)<.05 && v.paused && v.currentTime===window.savedTime})()`) {
				t.Fatal("inline video did not return to the reserved slot")
			}
		}
	}
	click(`[data-video-action="play"]`)
	wait(`!v.paused && !expandedVideoShell`)
	click("video")
	wait(`!!expandedVideoShell && !v.paused`)
	// Prefetch crosses the two text-heavy pages before the swipe. The next
	// video is prepared silently, and its existing element is promoted.
	wait(`state.postIDs.size===25 && document.querySelector('[data-post-id="100"] video')?.dataset.prefetch==='true'`)
	eval(`(()=>{window.preparedVideo=document.querySelector('[data-post-id="100"] video');return true})()`)
	if !eval(`preparedVideo.hidden && preparedVideo.paused && preparedVideo.preload==='metadata'`) {
		t.Fatal("speculative video became visible or played")
	}
	swipe(false)
	wait(`!!expandedVideoShell && expandedVideoShell!==v.closest('.video-shell') && state.postIDs.size===25`)
	wait(`expandedVideoShell.querySelector('video').readyState>=2 && !expandedVideoShell.querySelector('video').paused`)
	if !eval(`expandedVideoShell.querySelector('video')===preparedVideo && !preparedVideo.dataset.prefetch && !preparedVideo.hidden && v.paused && expandedVideoShell.closest('[data-post-id]').dataset.postId==='100' && document.body.style.overflow==='hidden'`) {
		t.Fatal("next video did not preserve timeline order")
	}
	eval(`(()=>{window.lastShell=expandedVideoShell;return true})()`)
	swipe(false)
	wait(`!videoSwipePending && !!expandedVideoShell && expandedVideoShell!==lastShell && expandedVideoShell.closest('[data-post-id]').dataset.postId==='90'`)
	eval(`(()=>{window.lastShell=expandedVideoShell;return true})()`)
	swipe(false)
	wait(`!videoSwipePending && expandedVideoShell===lastShell`)
	eval(`(()=>{location.hash='archive';return true})()`)
	wait(`state.view==='archive' && !expandedVideoShell && v.paused && !v.getAttribute('src') && document.body.style.overflow===''`)

	// Save independent playback preferences through the actual settings form.
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.view==='settings' && state.settings?.timeline_page_size===10 && $('timeline-page-size').value==='10'`)
	eval(`(()=>{$('settings-tab-display').click();$('video-auto-next').checked=true;$('video-timeline-autoplay').checked=true;$('video-loop').checked=true;$('media-preload-mode').value='off';$('archive-display-form').requestSubmit();return true})()`)
	wait(`state.settings.video_auto_next===true && state.settings.video_timeline_autoplay===true && state.settings.video_loop===true && state.settings.media_preload_mode==='off'`)
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && !state.postsLoading && document.querySelector('video')?.readyState>=2 && !document.querySelector('video').paused`)
	if !eval(`!expandedVideoShell && document.querySelector('video').muted && document.querySelector('video').loop && state.postIDs.size===10`) {
		detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({expanded:!!expandedVideoShell,muted:document.querySelector('video').muted,loop:document.querySelector('video').loop,count:state.postIDs.size,settings:state.settings})`))
		t.Fatal("viewport autoplay changed fullscreen, mute, loop or pagination", detail)
	}
	eval(`(()=>{window.autoFirst=document.querySelector('video');window.scrollTo(0,1200);return true})()`)
	wait(`autoFirst.paused`)
	eval(`(()=>{window.scrollTo(0,0);return true})()`)
	wait(`!autoFirst.paused`)
	// A background tab pauses and restores viewport playback.
	eval(`(()=>{Object.defineProperty(document,'hidden',{configurable:true,value:true});document.dispatchEvent(new Event('visibilitychange'));return true})()`)
	wait(`autoFirst.paused`)
	eval(`(()=>{delete document.hidden;document.dispatchEvent(new Event('visibilitychange'));return true})()`)
	wait(`!autoFirst.paused`)
	eval(`(()=>{$('image-dialog').showModal();return true})()`)
	wait(`autoFirst.paused`)
	eval(`(()=>{$('image-dialog').close();return true})()`)
	wait(`!autoFirst.paused`)
	// A user pause survives scrolling away and returning.
	eval(`(()=>{autoFirst.closest('.video-shell').querySelector('[data-video-action="play"]').click();window.scrollTo(0,1200);return true})()`)
	wait(`autoFirst.paused`)
	eval(`(()=>{window.scrollTo(0,0);return true})()`)
	wait(`timelineVideoSlots.get(autoFirst.closest('.video-slot'))>=.5`)
	if !eval(`autoFirst.paused`) {
		t.Fatal("scrolling resumed a manually paused video")
	}
	// Automatic fullscreen advance traverses text-only pages even with prefetch off.
	eval(`(()=>{autoFirst.closest('.video-shell').requestFullscreen=()=>Promise.reject(new Error('fixture'));autoFirst.click();autoFirst.currentTime=0;playTimelineVideo(autoFirst);return true})()`)
	if !eval(`!!expandedVideoShell && !autoFirst.loop`) {
		t.Fatal("fullscreen auto-next did not override looping")
	}
	wait(`expandedVideoShell?.closest('[data-post-id]').dataset.postId==='100' && !expandedVideoShell.querySelector('video').paused && state.postIDs.size===25`)
	wait(`expandedVideoShell?.closest('[data-post-id]').dataset.postId==='90' && !videoSwipePending`)
	wait(`expandedVideoShell?.querySelector('video').ended && !videoSwipePending`)
	if !eval(`expandedVideoShell.closest('[data-post-id]').dataset.postId==='90'`) {
		t.Fatal("last video wrapped instead of stopping")
	}
	// Disable both switches and verify the timeline returns to click-to-play.
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.view==='settings' && !expandedVideoShell`)
	eval(`(()=>{$('settings-tab-display').click();$('video-auto-next').checked=false;$('video-timeline-autoplay').checked=false;$('archive-display-form').requestSubmit();return true})()`)
	wait(`state.settings.video_auto_next===false && state.settings.video_timeline_autoplay===false`)
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home' && !state.postsLoading`)
	if !eval(`document.querySelector('video').paused && !document.querySelector('video').getAttribute('src')`) {
		t.Fatal("disabled viewport autoplay loaded a video")
	}
}
