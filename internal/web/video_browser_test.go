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
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({fullscreen:!!document.fullscreenElement,expanded:!!expandedVideoShell,video:window.v&&{time:v.currentTime,duration:v.duration,paused:v.paused,ready:v.readyState,error:v.error?.message},recordError:window.recordError,videoError:window.videoError,hit:window.clickHit,load:document.querySelector('.video-load')?.textContent,hidden:document.querySelector('.video-load')?.hidden,source:document.querySelector('video')?.getAttribute('src')})`))
				t.Fatal("UI timed out:", js, detail)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	touchClicks := false
	click := func(selector string) {
		t.Helper()
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
		// Denied/unsupported native fullscreen must still provide click-to-return.
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
			click("video")
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
	eval(`(()=>{location.hash='archive';return true})()`)
	wait(`state.view==='archive' && !expandedVideoShell && v.paused && !v.getAttribute('src') && document.body.style.overflow===''`)
}
