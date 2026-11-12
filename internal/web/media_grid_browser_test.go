package web

import (
	"context"
	"crypto/sha256"
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

func TestMixedMediaGridBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome media grid fixture")
	}
	a := testApp(t)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
		until := time.Now().Add(8 * time.Second)
		for !eval(js) {
			if time.Now().After(until) {
				t.Fatal("media grid timeout", js)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	// Record a real original locally, so fullscreen tests exercise actual playback.
	eval(`(()=>{const c=document.createElement('canvas');c.width=320;c.height=180;const x=c.getContext('2d'),s=c.captureStream(12),parts=[],r=new MediaRecorder(s,{mimeType:'video/mp4'});let n=0;const timer=setInterval(()=>{x.fillStyle=n++%2?'#234b3e':'#1d9bf0';x.fillRect(0,0,320,180)},80);r.ondataavailable=e=>parts.push(e.data);r.onstop=()=>{clearInterval(timer);s.getTracks().forEach(t=>t.stop());const f=new FileReader();f.onload=()=>window.gridVideo=f.result.split(',')[1];f.readAsDataURL(new Blob(parts,{type:'video/mp4'}))};r.start();setTimeout(()=>r.stop(),1200);return true})()`)
	wait(`!!window.gridVideo`)
	encoded, err := chromedp.Run(browser, chromedp.Evaluate[string](`window.gridVideo`))
	if err != nil {
		t.Fatal(err)
	}
	video, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	photo, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	cases := [][]string{{"video", "video", "video", "video"}, {"photo", "photo", "photo", "photo"}, {"photo", "video", "photo", "video"}, {"video", "video"}, {"video", "photo", "video"}, {"video"}}
	dir := filepath.Join(a.Config().DataDir, "media")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	for i, kinds := range cases {
		p := archive.Post{ID: fmt.Sprint(300 + i), Username: "alice", Text: "紧凑媒体布局"}
		for k, kind := range kinds {
			p.Assets = append(p.Assets, archive.Asset{ID: fmt.Sprint(k), Kind: kind})
		}
		if err := a.store.SavePosts("alice", []archive.Post{p}); err != nil {
			t.Fatal(err)
		}
		for k, kind := range kinds {
			data, ext := video, ".mp4"
			if kind == "photo" {
				data, ext = photo, ".png"
			}
			name := p.ID + "-" + fmt.Sprint(k) + ext
			if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.store.MediaResult(archive.MediaJob{PostID: p.ID, ID: fmt.Sprint(k), StorageID: "local"}, "media/"+name, int64(len(data)), fmt.Sprintf("%x", sha256.Sum256(data)), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated&&!state.postsLoading&&state.postIDs.size===6`)
	for _, width := range []int64{1280, 390, 320} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
			t.Fatal(err)
		}
		for _, theme := range []string{"light", "dark"} {
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			for i, kinds := range cases {
				eval(fmt.Sprintf(`(()=>{window.grid=document.querySelector('#posts>[data-post-id="%d"] .media-grid');grid.scrollIntoView({block:'center',behavior:'instant'});return true})()`, 300+i))
				wait(`[...grid.querySelectorAll('img')].every(i=>i.complete&&i.naturalWidth>0)`)
				if !eval(`(()=>{const g=grid.getBoundingClientRect(),cells=[...grid.children].map(c=>c.getBoundingClientRect());return Math.abs(g.width/g.height-16/9)<.03&&cells.every(c=>c.width>20&&c.height>20&&c.left>=g.left&&c.right<=g.right+1&&c.top>=g.top&&c.bottom<=g.bottom+1)&&document.documentElement.scrollWidth<=innerWidth})()`) {
					t.Fatalf("grid overflow or size: width=%d theme=%s case=%d", width, theme, i)
				}
				if len(kinds) == 4 && !eval(`(()=>{const r=[...grid.children].map(c=>c.getBoundingClientRect());return Math.abs(r[0].top-r[1].top)<1&&r[1].left>r[0].right&&r[2].top>r[0].bottom&&Math.abs(r[2].top-r[3].top)<1&&Math.abs(r[0].height-r[3].height)<1})()`) {
					t.Fatal("four media are not a 2x2 grid", width, i)
				}
				if len(kinds) == 3 && !eval(`(()=>{const r=[...grid.children].map(c=>c.getBoundingClientRect());return r[0].height>r[1].height*1.9&&r[2].top>r[1].bottom&&Math.abs(r[1].left-r[2].left)<1})()`) {
					t.Fatal("three media layout", width)
				}
				if screenshotDir := os.Getenv("XM_UI_SCREENSHOT_DIR"); screenshotDir != "" && i == 0 && theme == "dark" {
					shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(screenshotDir, fmt.Sprintf("media-grid-%d.png", width)), shot, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		// A video still fills the viewport; returning restores the same grid size.
		eval(`(()=>{window.grid=document.querySelector('#posts>[data-post-id="300"] .media-grid');window.gridHeight=grid.getBoundingClientRect().height;const shell=grid.querySelector('.video-shell');shell.requestFullscreen=()=>Promise.reject(new Error('test fallback'));grid.scrollIntoView({block:'center',behavior:'instant'});shell.querySelector('.video-load').click();return true})()`)
		wait(`!!expandedVideoShell&&expandedVideoShell.querySelector('video').readyState>=2`)
		if !eval(`(()=>{const s=expandedVideoShell,r=s.getBoundingClientRect();return Math.abs(r.width-innerWidth)<1&&Math.abs(r.height-innerHeight)<1&&getComputedStyle(s.querySelector('.video-seek')).display!=='none'&&getComputedStyle(s.querySelector('[data-video-action="mute"]')).display!=='none'&&Math.abs(grid.getBoundingClientRect().height-gridHeight)<1})()`) {
			t.Fatal("fullscreen clipped to tile or missing controls", width)
		}
		eval(`(()=>{collapseExpandedVideo();return true})()`)
		if !eval(`(()=>{const s=grid.querySelector('.video-shell'),r=s.getBoundingClientRect(),controls=[...s.querySelectorAll('.video-controls button')].filter(b=>getComputedStyle(b).display!=='none');return Math.abs(grid.getBoundingClientRect().height-gridHeight)<1&&controls.length===2&&controls.every(b=>{const c=b.getBoundingClientRect();return c.left>=r.left&&c.right<=r.right+1})})()`) {
			t.Fatal("inline playback controls overflow", width)
		}
		eval(`(()=>{releaseTimelineMedia();document.querySelector('#posts>[data-post-id="302"] .media-button').click();return true})()`)
		wait(`$('image-dialog').open&&$('large-image').complete&&$('large-image').naturalWidth>0`)
		if !eval(`[...document.querySelectorAll('#posts>[data-post-id="302"] [data-image]')].every(b=>imageGallery.includes(b.dataset.image))&&$('large-image').getAttribute('src')===document.querySelector('#posts>[data-post-id="302"] [data-image]').dataset.image&&getComputedStyle($('large-image')).objectFit==='contain'`) {
			t.Fatal("mixed grid lost original image gallery")
		}
		eval(`(()=>{$('image-dialog').close();return true})()`)
	}
}
