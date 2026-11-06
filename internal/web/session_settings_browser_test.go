package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestSessionSettingsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
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
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`document.getElementById('toast')?.textContent||document.body.innerText.slice(-1000)`))
				t.Fatal("session settings UI timed out", js, detail)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined'`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)

	wait(`state.authenticated`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.view==='settings' && $('session-ttl-seconds').value==='43200' && state.settings?.session_ttl_seconds===43200`)
	eval(`(()=>{$('session-ttl-seconds').value='604800';$('session-ttl-seconds').dispatchEvent(new Event('change',{bubbles:true}));return true})()`)
	if !eval(`!$('settings-draft-notice').hidden`) {
		t.Fatal("login settings draft not tracked")
	}
	eval(`(()=>{location.hash='home';return true})()`)
	wait(`state.view==='home'`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.view==='settings' && !$('settings-account').hidden`)
	if !eval(`$('session-ttl-seconds').value==='604800' && state.settings.session_ttl_seconds===43200`) {
		t.Fatal("navigation lost unsaved login settings")
	}
	eval(`(()=>{$('session-settings-form').requestSubmit();return true})()`)
	wait(`state.settings.session_ttl_seconds===604800 && $('session-settings-result').textContent.includes('下次登录生效')`)
	if !eval(`!settingsFormDirty('session-settings-form') && state.authenticated`) {
		t.Fatal("save did not clear draft or invalidated current login")
	}
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 844)); err != nil {
		t.Fatal(err)
	}
	if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
		t.Fatal("login settings overflow on mobile")
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#settings")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && state.authenticated && $('session-ttl-seconds').value==='604800'`)
	eval(`(()=>{$('logout').click();return true})()`)
	wait(`!state.authenticated && !$('login-view').hidden`)
	if !eval(`$('session-ttl-seconds').value==='43200' && $('session-settings-result').textContent===''`) {
		t.Fatal("logout did not clear login settings form")
	}
}
