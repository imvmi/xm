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

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestTimelinePhotoLayoutsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome photo layout fixture")
	}
	a := testApp(t)
	sizes := []image.Point{{1200, 700}, {800, 800}, {900, 1600}, {400, 2000}, {2000, 400}}
	payloads := make([][]byte, len(sizes))
	for i, size := range sizes {
		img := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
		for y := 0; y < size.Y; y++ {
			for x := 0; x < size.X; x++ {
				c := color.RGBA{R: uint8(40 + x*100/size.X), G: uint8(80 + y*100/size.Y), B: 170, A: 255}
				if x < 20 || y < 20 || x >= size.X-20 || y >= size.Y-20 {
					c = color.RGBA{240, 120, 50, 255}
				}
				img.SetRGBA(x, y, c)
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		payloads[i] = b.Bytes()
	}
	dir := filepath.Join(a.Config().DataDir, "media")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 8; n++ {
		id := fmt.Sprint(100 + n)
		count := 1
		if n > 5 {
			count = n - 4
		}
		p := archive.Post{ID: id, Username: "alice", AuthorID: "42", Text: fmt.Sprintf("图片布局 %d", n)}
		for k := 0; k < count; k++ {
			p.Assets = append(p.Assets, archive.Asset{ID: fmt.Sprint(k), Kind: "photo"})
		}
		if err := a.store.SavePosts("alice", []archive.Post{p}); err != nil {
			t.Fatal(err)
		}
		for k := 0; k < count; k++ {
			index := n - 1
			if n > 5 {
				index = k % 3
			}
			data := payloads[index]
			name := id + "-" + fmt.Sprint(k) + ".png"
			if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.store.MediaResult(archive.MediaJob{PostID: id, ID: fmt.Sprint(k), StorageID: "local"}, "media/"+name, int64(len(data)), fmt.Sprintf("%x", sha256.Sum256(data)), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
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
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({hash:location.hash,view:state.view,open:$('image-dialog').open,index:imageIndex,src:$('large-image').getAttribute('src'),loaded:$('large-image').naturalWidth,grid:window.fixtureGrid?.outerHTML})`))
				t.Fatal("UI timeout", js, detail)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(1280, 900), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof layoutTimelinePhoto==='function' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated&&!state.postsLoading&&state.postIDs.size===8`)
	for _, route := range []string{"home", "user/alice"} {
		eval(fmt.Sprintf(`(()=>{location.hash=%q;return true})()`, route))
		wait(fmt.Sprintf(`!state.postsLoading&&state.postIDs.size===8&&state.view===%q`, map[bool]string{true: "home", false: "user"}[route == "home"]))
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 8; n++ {
				selector := fmt.Sprintf(`#posts > [data-post-id="%d"] .photo-grid`, 100+n)
				eval(fmt.Sprintf(`(()=>{window.fixtureGrid=document.querySelector(%q);fixtureGrid.scrollIntoView({block:'center',behavior:'instant'});return true})()`, selector))
				wait(`[...fixtureGrid.querySelectorAll('img')].every(i=>i.complete&&i.naturalWidth>0)`)
				if !eval(`(()=>{const g=fixtureGrid.getBoundingClientRect();return g.width>0 && g.height>0 && g.left>=0 && g.right<=innerWidth+1 && document.documentElement.scrollWidth<=innerWidth})()`) {
					t.Fatalf("overflow %s width %d post %d", route, width, n)
				}
				if n <= 3 {
					if !eval(`(()=>{const i=fixtureGrid.querySelector('img'),r=i.getBoundingClientRect();return getComputedStyle(i).objectFit==='contain'&&r.height<=520.5&&!fixtureGrid.querySelector('.photo-preview-hint')})()`) {
						t.Fatalf("ordinary photo cropped %s %d %d", route, width, n)
					}
				} else if n <= 5 {
					wait(`fixtureGrid.querySelector('.photo-preview-hint')!==null`)
					if !eval(`(()=>{const b=fixtureGrid.querySelector('button'),i=b.querySelector('img'),r=b.getBoundingClientRect(),ir=i.getBoundingClientRect();return r.height<=520.5&&Math.abs(ir.height-r.height)<2&&getComputedStyle(i).objectFit==='cover'&&b.textContent.includes('点击查看完整图片')})()`) {
						t.Fatalf("extreme preview %s %d %d", route, width, n)
					}
					eval(`(()=>{fixtureGrid.querySelector('button').click();return true})()`)
					wait(`$('image-dialog').open&&$('large-image').complete&&$('large-image').naturalWidth>0`)
					if !eval(`(()=>{const i=$('large-image'),r=i.getBoundingClientRect();return getComputedStyle(i).objectFit==='contain'&&r.width<=innerWidth+1&&r.height<=innerHeight+1&&r.left>=-1&&r.top>=-1})()`) {
						detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`(()=>{const i=$('large-image'),r=i.getBoundingClientRect();return JSON.stringify({route:location.hash,width:innerWidth,src:i.src,nw:i.naturalWidth,nh:i.naturalHeight,r:r.toJSON(),fit:getComputedStyle(i).objectFit})})()`))
						t.Fatal("full image was cropped", detail)
					}
					eval(`(()=>{$('image-dialog').close();return true})()`)
					wait(`!$('image-dialog').open && imageGallery.length===0`)
				} else {
					if !eval(`(()=>{const g=fixtureGrid.getBoundingClientRect(),b=[...fixtureGrid.children].map(x=>x.getBoundingClientRect());return Math.abs(g.width/g.height-16/9)<.02&&b.every(r=>r.height>20&&r.top>=g.top&&r.bottom<=g.bottom+1)})()`) {
						t.Fatalf("grid geometry %s %d %d", route, width, n)
					}
					if n == 7 && !eval(`(()=>{const r=[...fixtureGrid.children].map(x=>x.getBoundingClientRect());return r[0].height>r[1].height*1.9&&Math.abs(r[1].x-r[2].x)<1&&r[2].top>r[1].bottom})()`) {
						t.Fatal("three-photo layout is not left-large/right-stacked")
					}
				}
				if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" && route == "home" && (n == 3 || n == 7) {
					shot, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("photos-%d-%d.png", width, n)), shot, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
}
