package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestBrowserSettingsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline browser settings UI fixture")
	}
	a := testApp(t)
	type diagnosis struct {
		cfg     archive.Config
		cookies bool
	}
	diagnoses := make(chan diagnosis, 8)
	canceled := make(chan struct{}, 3)
	a.checkChrome = func(ctx context.Context, cfg archive.Config, cookies *archive.Cookies) (archive.ChromeInfo, error) {
		diagnoses <- diagnosis{cfg, cookies != nil}
		if cfg.BrowserMode == "cdp" && cfg.CDPPort == 5820 {
			<-ctx.Done()
			canceled <- struct{}{}
			return archive.ChromeInfo{}, ctx.Err()
		}
		if cfg.CDPPort == 5819 {
			return archive.ChromeInfo{}, errors.New("CDP 连接被拒绝，请检查调试端口")
		}
		return archive.ChromeInfo{Product: "Chrome/fixture", Headless: true}, nil
	}
	if err := a.store.SaveCookies(archive.Cookies{AuthToken: "fixture", CSRF: "fixture"}); err != nil {
		t.Fatal(err)
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
		for !eval(js) {
			select {
			case <-ctx.Done():
				t.Fatal("UI timed out", js)
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !$('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`state.authenticated`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`!$('view-settings').hidden && settingsBaselines.has('browser-settings-form')`)
	eval(`(()=>{$('settings-tab-collection').click();return true})()`)
	if !eval(`$('browser-mode').value==='local' && $('cdp-fields').hidden && $('cdp-host').disabled && !$('chrome-mode').disabled`) {
		t.Fatal("local defaults missing")
	}
	eval(`(()=>{$('browser-mode').value='cdp';$('browser-mode').dispatchEvent(new Event('change',{bubbles:true}));$('cdp-host').value='192.0.2.10';$('cdp-port').value='5818';$('cdp-port').dispatchEvent(new Event('input',{bubbles:true}));return true})()`)
	if !eval(`!$('cdp-fields').hidden && !$('cdp-host').disabled && $('chrome-mode').disabled && $('chrome-mode-state').textContent==='远端管理' && settingsFormDirty('browser-settings-form')`) {
		t.Fatal("CDP fields not editable")
	}
	eval(`(()=>{void refreshSettings();return true})()`)
	if !eval(`$('cdp-port').value==='5818' && $('browser-mode').value==='cdp'`) {
		t.Fatal("refresh lost unsaved browser draft")
	}
	// Both entry points test the selected, unsaved environment. A connection
	// check does not import cookies; the full diagnostic still verifies login.
	for _, button := range []string{"test-browser-connection", "test-chrome"} {
		result := "browser-connection-result"
		if button == "test-chrome" {
			result = "chrome-result"
		}
		eval(`(()=>{$('` + button + `').click();return true})()`)
		wait(`!chromeDiagnosticPending && $('` + result + `').textContent.includes('耗时：')`)
		got := <-diagnoses
		if got.cfg.BrowserMode != "cdp" || got.cfg.CDPHost != "192.0.2.10" || got.cfg.CDPPort != 5818 || got.cookies != (button == "test-chrome") {
			t.Fatal("diagnostic ignored unsaved browser selection")
		}
		if a.Config().BrowserMode != "local" || !eval(`settingsFormDirty('browser-settings-form') && $('`+result+`').textContent.includes('CDP 192.0.2.10:5818')`) {
			t.Fatal("diagnostic persisted/discarded draft or omitted tested endpoint")
		}
	}
	eval(`(()=>{$('cdp-port').value='5819';$('test-browser-connection').click();return true})()`)
	wait(`!chromeDiagnosticPending && $('browser-connection-result').textContent.includes('连接被拒绝')`)
	<-diagnoses
	if !eval(`$('browser-connection-result').classList.contains('error') && !$('test-browser-connection').disabled && !$('test-chrome').disabled`) {
		t.Fatal("connection failure not visible or retry disabled")
	}
	eval(`(()=>{$('cdp-port').value='5818';return true})()`)
	eval(`(()=>{$('save-browser-settings').click();return true})()`)
	wait(`state.settings.browser_mode==='cdp' && !settingsFormDirty('browser-settings-form') && !$('save-browser-settings').disabled`)
	if !eval(`$('chrome-mode').disabled && $('chrome-mode-state').textContent==='远端管理'`) {
		t.Fatal("local headless toggle enabled for remote browser")
	}
	if a.Config().CDPHost != "192.0.2.10" || a.Config().CDPPort != 5818 {
		t.Fatal("browser settings not saved")
	}
	// A server rejection preserves the saved configuration and the editable draft.
	eval(`(()=>{$('cdp-host').value='http://bad';$('cdp-host').dispatchEvent(new Event('input',{bubbles:true}));$('save-browser-settings').click();return true})()`)
	wait(`!$('save-browser-settings').disabled && !$('toast').hidden && $('toast').textContent.includes('CDP 地址')`)
	if a.Config().CDPHost != "192.0.2.10" || !eval(`settingsFormDirty('browser-settings-form')`) {
		t.Fatal("failed save discarded saved state or draft")
	}
	eval(`(()=>{$('browser-mode').value='local';$('browser-mode').dispatchEvent(new Event('change',{bubbles:true}));$('test-browser-connection').click();return true})()`)
	wait(`!chromeDiagnosticPending && $('browser-connection-result').textContent.includes('测试配置：本地 Chrome') && $('browser-connection-result').textContent.includes('耗时：')`)
	if got := <-diagnoses; got.cfg.BrowserMode != "local" || a.Config().BrowserMode != "cdp" {
		t.Fatal("local draft diagnosis used or modified saved mode")
	}
	eval(`(()=>{$('save-browser-settings').click();return true})()`)
	wait(`state.settings.browser_mode==='local' && !$('chrome-mode').disabled && !settingsFormDirty('browser-settings-form')`)
	if a.Config().CDPPort != 5818 || !eval(`$('cdp-fields').hidden`) {
		t.Fatal("switching back lost remote settings")
	}
	for _, action := range []string{"mode", "navigation", "logout"} {
		eval(`(()=>{$('browser-mode').value='cdp';$('browser-mode').dispatchEvent(new Event('change',{bubbles:true}));$('cdp-host').value='127.0.0.1';$('cdp-port').value='5820';$('cdp-port').dispatchEvent(new Event('input',{bubbles:true}));$('test-browser-connection').click();return true})()`)
		wait(`chromeDiagnosticPending && $('test-chrome').disabled`)
		select {
		case <-diagnoses:
		case <-ctx.Done():
			t.Fatal("diagnosis did not start")
		}
		switch action {
		case "mode":
			eval(`(()=>{$('browser-mode').value='local';$('browser-mode').dispatchEvent(new Event('change',{bubbles:true}));return true})()`)
		case "navigation":
			eval(`(()=>{location.hash='home';return true})()`)
		case "logout":
			eval(`(()=>{$('logout').click();return true})()`)
		}
		select {
		case <-canceled:
		case <-ctx.Done():
			t.Fatal("diagnostic HTTP request was not canceled", action)
		}
		wait(`!chromeDiagnosticPending && $('browser-connection-result').textContent===''`)
		if action == "mode" && !eval(`!$('chrome-mode').disabled && $('chrome-mode-state').textContent==='Headless'`) {
			t.Fatal("local draft inherited saved remote controls")
		}
		if action == "navigation" {
			eval(`(()=>{location.hash='settings';return true})()`)
			wait(`!$('view-settings').hidden`)
		}
	}
}
