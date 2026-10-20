package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestThemeAndHomeBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	cfg := a.Config()
	cfg.ArchiveTheme = "dark"
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	quote := archive.Post{ID: "50", Username: "bob", Text: "引用内容也应保持清晰可读。", Raw: json.RawMessage(`{}`)}
	posts := []archive.Post{}
	for i := 0; i < 32; i++ {
		posts = append(posts, archive.Post{ID: fmt.Sprint(100 + i), Username: "alice", Text: "这是已采集的推文。\n时间线应有舒适的底部留白，夜间模式下文字和引用卡片都能清楚阅读。", CreatedAt: "2026-10-19T10:00:00Z", Raw: json.RawMessage(`{}`), Quote: &quote})
	}
	if err := a.store.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("bob", []archive.Post{{ID: "200", Username: "bob", Text: "来自另一个账号的回复，同样出现在首页。", ReplyTo: "1", Raw: json.RawMessage(`{}`), CreatedAt: "2026-10-19T11:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	alloc, closeAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer closeAlloc()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
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
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`JSON.stringify({view:typeof state==='undefined'?'no state':state.view,count:typeof state==='undefined'?0:state.postIDs.size,loading:typeof state==='undefined'?null:state.postsLoading,scroll:scrollY,width:innerWidth,login:document.getElementById('login-error')?.textContent,toast:document.getElementById('toast')?.textContent})`))
				t.Fatalf("UI timed out %s: %s", js, detail)
			}
			select {
			case <-ctx.Done():
				t.Fatal("UI timed out", js)
			case <-time.After(30 * time.Millisecond):
			}
		}
	}
	media := func(value string) chromedp.Action[chromedp.Void] {
		return chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
			_, err := cdp.Call(ctx, target, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{Features: []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: value}}})
			return err
		})
	}
	if err := chromedp.Do(browser, media("light"), chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof applyArchiveTheme==='function' && document.documentElement.dataset.theme==='dark'`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`typeof state!=='undefined' && state.view==='home' && state.postIDs.size===24 && !state.postsLoading`)
	if !eval(`$('posts').textContent.includes('另一个账号的回复') && $('home-feed').contains($('timeline-panel')) && document.querySelector('[data-view=home]').getAttribute('aria-current')==='page'`) {
		t.Fatal("default home missing multi-account timeline")
	}
	for _, width := range []int64{1280, 390} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 844)); err != nil {
			t.Fatal(err)
		}
		for _, view := range []string{"home", "user/alice", "archive", "monitors", "activity", "notifications", "settings"} {
			eval(`(()=>{location.hash=` + fmt.Sprintf("%q", view) + `;return true})()`)
			name := view
			if view == "monitors" {
				name = "archive"
			}
			if view == "user/alice" {
				name = "user"
			}
			wait(fmt.Sprintf(`state.view===%q && !$('view-'+state.view).hidden`, name))
			if name == "home" || name == "user" {
				wait(`state.postIDs.size>0 && !state.postsLoading && !!document.querySelector('#posts .post-body')`)
				for _, selector := range []string{".timeline-top-button", "[data-timeline-top]"} {
					if !eval(`(()=>{window.scrollTo({top:500,behavior:'instant'});window.topClickSnapshot={request:state.postRequest,tab:state.timelineTab,first:$('posts').firstElementChild};return scrollY>0})()`) {
						t.Fatal("timeline fixture cannot scroll", view, width)
					}
					eval(`(()=>{requestAnimationFrame(()=>requestAnimationFrame(()=>document.querySelector('#view-'+state.view+' ` + selector + `').click()));return true})()`)
					wait(`scrollY===0`)
					if !eval(`state.postRequest===topClickSnapshot.request && state.timelineTab===topClickSnapshot.tab && $('posts').firstElementChild===topClickSnapshot.first`) {
						t.Fatal("return to top reloaded or changed timeline", view, width)
					}
				}
				if !eval(`(()=>{const tab=document.querySelector('#view-'+state.view+' .timeline-tabs button.selected'),post=document.querySelector('#posts .post');return getComputedStyle(tab).color==='rgb(231, 233, 234)' && getComputedStyle(tab,'::after').backgroundColor==='rgb(29, 155, 240)' && getComputedStyle(post).backgroundColor==='rgb(0, 0, 0)' && getComputedStyle(post).borderBottomColor==='rgb(47, 51, 54)'})()`) {
					t.Fatal("Twitter-style dark palette or selected tab contrast", view, width)
				}
			}
			if name == "settings" {
				wait(`$('archive-theme').value==='dark'`)
				eval(`(()=>{$('settings-tab-storage').click();return true})()`)
				wait(`$('storage-readonly-name').textContent==='本地文件系统'`)
				if !eval(`$('remote-cache-panel').hidden && $('remote-cache-note').hidden && !$('remote-cache-panel').getBoundingClientRect().height && !$('remote-cache-note').getBoundingClientRect().height`) {
					t.Fatal("remote cache controls visible for local storage", width)
				}
				eval(`(()=>{$('settings-tab-display').click();return true})()`)
			}
			if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatalf("overflow %s at %d", view, width)
			}
			if !eval(`(()=>{const root=$('view-'+state.view);const nodes=[...root.querySelectorAll('.profile-feed,.post,.quote-card,.user-card,.panel,.job-list,.notification-empty'),document.querySelector('.sidebar nav')];return nodes.filter(el=>el.getBoundingClientRect().width).every(el=>{const c=getComputedStyle(el).backgroundColor.match(/[\d.]+/g);return !c || Number(c[0])<100})})()`) {
				t.Fatalf("light surface in dark %s at %d", view, width)
			}
			if name == "home" || name == "user" {
				if !eval(`getComputedStyle(document.querySelector('#app>main')).backgroundColor==='rgb(0, 0, 0)' && getComputedStyle(document.querySelector('.sidebar nav a.active')).color==='rgb(231, 233, 234)'`) {
					t.Fatal("reading page or navigation overrides theme", view, width)
				}
				if !eval(`(()=>{const p=document.querySelector('#posts .post-body');return parseFloat(getComputedStyle(p).paddingBottom)<=14 && parseFloat(getComputedStyle(p).paddingBottom)>=12 && parseFloat(getComputedStyle(document.querySelector('#posts .post-text')).lineHeight)/parseFloat(getComputedStyle(document.querySelector('#posts .post-text')).fontSize)<=1.46 && getComputedStyle(document.querySelector('#posts .post-text')).color==='rgb(231, 233, 234)'})()`) {
					t.Fatal("post spacing or contrast", view, width)
				}
			}
			if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" && (name == "home" || name == "user" || name == "settings") {
				image, err := chromedp.Run(browser, chromedp.CaptureScreenshot())
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-dark-%d.png", name, width)), image, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Save light while the OS is dark; reload directly into home, not settings.
	if err := chromedp.Do(browser, media("dark")); err != nil {
		t.Fatal(err)
	}
	eval(`(()=>{$('settings-tab-display').click();$('archive-theme').value='light';$('archive-display-form').requestSubmit();return true})()`)
	wait(`state.settings.archive_theme==='light' && document.documentElement.dataset.theme==='light'`)
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#home")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && state.authenticated && state.view==='home' && state.postIDs.size>0 && document.documentElement.dataset.theme==='light'`)
	if !eval(`getComputedStyle(document.querySelector('#posts .post')).backgroundColor==='rgb(255, 255, 255)'`) {
		t.Fatal("light mode not restored")
	}
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.view==='settings' && $('archive-theme').value==='light'`)
	eval(`(()=>{$('settings-tab-display').click();$('archive-theme').value='system';$('archive-display-form').requestSubmit();return true})()`)
	wait(`state.settings.archive_theme==='system' && document.documentElement.dataset.theme==='dark'`)
	if err := chromedp.Do(browser, media("light")); err != nil {
		t.Fatal(err)
	}
	wait(`document.documentElement.dataset.theme==='light'`)
}
