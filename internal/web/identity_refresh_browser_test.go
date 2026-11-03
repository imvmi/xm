package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestAccountIdentityRefreshBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome identity UI fixture")
	}
	a := identityRefreshFixture(t)
	var mode atomic.Int32
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	a.refreshIdentity = func(ctx context.Context, _ archive.Config, _ archive.Cookies, _ archive.TargetIdentity, _ []string, username string) (archive.Profile, error) {
		if mode.Load() == 2 {
			started <- struct{}{}
			<-ctx.Done()
			cancelled <- struct{}{}
			return archive.Profile{}, ctx.Err()
		}
		if username == "" && mode.Load() == 0 {
			return archive.Profile{}, errors.New("历史推文不可访问，请填写当前用户名")
		}
		if username == "other" {
			return archive.Profile{ID: "99", Username: "other"}, nil
		}
		return archive.Profile{ID: "42", Username: "newalice"}, nil
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
				t.Fatal("UI timeout", js)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	open := func() {
		t.Helper()
		eval(`(()=>{document.querySelector('[data-account=alice]').click();return true})()`)
		wait(`$('account-menu-dialog').open`)
		eval(`(()=>{document.querySelector('#account-menu-content [data-op=identity]').click();return true})()`)
		wait(`$('identity-dialog').open`)
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#accounts")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !$('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated && state.monitors.length===1 && !!document.querySelector('[data-account=alice]')`)
	open()
	wait(`!identityController && !$('identity-form').hidden && $('identity-result').textContent.includes('历史推文不可访问')`)
	if !eval(`$('identity-account').textContent.includes('数字 ID：42') && !$('account-menu-dialog').open`) {
		t.Fatal("identity modal missing saved ID")
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(320, 700)); err != nil {
		t.Fatal(err)
	}
	if !eval(`(()=>{const d=$('identity-dialog'),r=d.getBoundingClientRect();return r.left>=0&&r.right<=innerWidth&&r.bottom<=innerHeight&&d.scrollWidth<=d.clientWidth+1})()`) {
		t.Fatal("mobile identity dialog overflow")
	}
	eval(`(()=>{$('identity-username').value='other';$('identity-submit').click();return true})()`)
	wait(`!identityController && $('identity-result').textContent.includes('账号身份不一致')`)
	eval(`(()=>{$('identity-username').value='https://x.com/newalice';$('identity-submit').click();return true})()`)
	wait(`!identityController && $('identity-result').textContent.includes('已更新为 @newalice') && state.monitors[0].current_username==='newalice'`)
	if !eval(`$('identity-form').hidden && $('identity-result').textContent.includes('自动采集仍暂停') && state.monitors[0].username==='alice' && state.monitors[0].user_id==='42' && !state.monitors[0].enabled && state.monitors[0].interval_override_seconds===300 && !state.monitors[0].show_in_timeline`) {
		t.Fatal("identity refresh changed settings")
	}
	eval(`(()=>{$('identity-dialog').close();return true})()`)
	wait(`!$('identity-dialog').open`)
	mode.Store(1)
	open()
	wait(`!identityController && $('identity-result').textContent.includes('用户名未变化')`)
	eval(`(()=>{$('identity-dialog').close();return true})()`)
	wait(`!$('identity-dialog').open`)
	mode.Store(2)
	open()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("lookup did not start")
	}
	if !eval(`$('identity-auto').disabled && $('identity-submit').disabled`) {
		t.Fatal("duplicate lookup enabled")
	}
	eval(`(()=>{$('identity-dialog').close();return true})()`)
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("closing dialog did not cancel lookup")
	}
	wait(`!identityController && !$('identity-dialog').open`)
}
