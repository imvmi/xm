package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestTimelineAutoplayWithoutGestureBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Flag("autoplay-policy", "document-user-activation-required"))
	alloc, stopAlloc := chromedp.NewExecAllocator(ctx, options...)
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
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({active:navigator.userActivation.hasBeenActive,v:[...document.querySelectorAll('video')].map(v=>({paused:v.paused,muted:v.muted,ready:v.readyState,src:v.getAttribute('src'),error:v.error?.message})),settings:state.settings})`))
				t.Fatal("autoplay timed out", js, detail)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	// Record real video WITH an audio track. Silent canvas-only fixtures cannot
	// reproduce browser restrictions on unmuted autoplay.
	eval(`(()=>{const b=document.createElement('button');b.id='record-fixture';b.textContent='Record';b.style='position:fixed;inset:0;z-index:9999';b.onclick=async()=>{
 const audio=new AudioContext();await audio.resume();const oscillator=audio.createOscillator(),destination=audio.createMediaStreamDestination();oscillator.connect(destination);oscillator.start();
 const canvas=document.createElement('canvas');canvas.width=320;canvas.height=180;const c=canvas.getContext('2d'),stream=canvas.captureStream(12);stream.addTrack(destination.stream.getAudioTracks()[0]);
 const chunks=[],recorder=new MediaRecorder(stream,{mimeType:'video/mp4'});let frame=0;const timer=setInterval(()=>{c.fillStyle=frame++%2?'#234b3e':'#1d9bf0';c.fillRect(0,0,320,180)},80);
 recorder.ondataavailable=e=>chunks.push(e.data);recorder.onstop=()=>{clearInterval(timer);stream.getTracks().forEach(t=>t.stop());audio.close();const reader=new FileReader();reader.onload=()=>window.recordedVideo=reader.result.split(',')[1];reader.readAsDataURL(new Blob(chunks,{type:'video/mp4'}))};recorder.start();setTimeout(()=>recorder.stop(),1800);
 };document.body.append(b);return true})()`)
	if err := chromedp.Do(browser, chromedp.Click("#record-fixture")); err != nil {
		t.Fatal(err)
	}
	wait(`!!window.recordedVideo`)
	encoded, err := chromedp.Run(browser, chromedp.Evaluate[string](`window.recordedVideo`))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	seedCachedMedia(t, a, "100", "video", ".mp4", payload)
	seedCachedMedia(t, a, "90", "video", ".mp4", payload)
	for i := 0; i < 12; i++ {
		if err := a.store.SavePosts("alice", []archive.Post{{ID: strconv.Itoa(200 + i), Username: "alice", Text: "scroll past this post"}}); err != nil {
			t.Fatal(err)
		}
	}
	stopBrowser()
	stopAlloc()
	for _, scenario := range []struct {
		muted bool
		width int64
	}{{true, 390}, {false, 390}, {true, 1280}, {false, 1280}} {
		muted := scenario.muted
		t.Logf("autoplay: muted=%v viewport=%d", muted, scenario.width)
		cfg := a.Config()
		cfg.VideoTimelineAutoplay = true
		cfg.VideoDefaultMuted = &muted
		cfg.VideoLoop = true
		cfg.MediaPreloadMode = "off"
		cfg.TimelinePageSize = 10
		if err := a.saveConfig(cfg); err != nil {
			t.Fatal(err)
		}
		alloc, stopAlloc = chromedp.NewExecAllocator(ctx, options...)
		defer stopAlloc()
		browser, stopBrowser = chromedp.NewContext(alloc)
		defer stopBrowser()
		if err := chromedp.Do(browser, chromedp.EmulateViewport(scenario.width, 844), chromedp.Navigate(server.URL)); err != nil {
			t.Fatal(err)
		}
		wait(`typeof state!=='undefined' && !!$('username')`)
		eval(`(()=>{document.styleSheets[0].insertRule('#posts>.post{min-height:700px}',document.styleSheets[0].cssRules.length);return true})()`)
		password, _ := json.Marshal(testPassword)
		eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
		wait(`state.authenticated && !state.postsLoading && state.postIDs.size===10`)
		if !eval(`!navigator.userActivation.hasBeenActive && !document.querySelector('video')`) {
			t.Fatal("fixture unlocked autoplay or loaded video before scrolling")
		}
		// Scroll to the next page and then to its first video without a trusted click.
		eval(`(()=>{window.scrollTo({top:document.documentElement.scrollHeight,behavior:'instant'});return true})()`)
		wait(`!state.postsLoading && !!document.querySelector('video')`)
		eval(`(()=>{document.querySelector('.video-slot').scrollIntoView({block:'center',behavior:'instant'});return true})()`)
		wait(`document.querySelector('video').readyState>=2 && !document.querySelector('video').paused`)
		if !eval(`!expandedVideoShell && document.querySelector('video').muted && !navigator.userActivation.hasBeenActive`) {
			t.Fatal("automatic playback needs a gesture or entered fullscreen")
		}
		eval(`(()=>{window.firstAuto=document.querySelector('video');document.querySelectorAll('.video-slot')[1].scrollIntoView({block:'center',behavior:'instant'});return true})()`)
		wait(`firstAuto.paused && !document.querySelectorAll('video')[1].paused`)
		// A muted fallback must not change the user's manual playback preference.
		if a.Config().VideoMutedByDefault() != muted {
			t.Fatal("autoplay changed saved sound preference")
		}
		stopBrowser()
		stopAlloc()
	}
}
