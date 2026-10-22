package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestAPICredentialsBrowser(t *testing.T) {
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
		for !eval(js) {
			select {
			case <-ctx.Done():
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`document.getElementById('toast')?.textContent||document.body.innerText.slice(0,300)`))
				t.Fatal("credential UI timed out", js, detail)
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#settings/credentials")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && !!$('username')`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`state.authenticated && !$('settings-credentials').hidden && $('credential-list').textContent.includes('尚无 API 凭据')`)
	if !eval(`document.querySelector('#settings-credentials a[href="/docs/api"]')!==null`) {
		t.Fatal("docs guide missing")
	}
	eval(`(()=>{$('credential-new').click();$('credential-name').value='Integration';$('credential-accounts').value='alice';return true})()`)
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int64{1280, 390} {
			if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
				t.Fatal(err)
			}
			eval(fmt.Sprintf(`(()=>{applyArchiveTheme(%q);return true})()`, theme))
			if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
				t.Fatalf("credentials layout overflow %s %d", theme, width)
			}
		}
	}
	eval(`(()=>{document.querySelector('#credential-form button[type="submit"]').click();return true})()`)
	wait(`$('credential-secret-dialog').open && $('credential-secret').value.startsWith('xm_')`)
	eval(`(()=>{window.testFirstSecret=$('credential-secret').value;document.querySelector('[data-close="credential-secret-dialog"]').click();return true})()`)
	wait(`$('credential-secret').value==='' && $('credential-list').textContent.includes('Integration')`)
	eval(`(()=>{document.querySelector('[data-credential-action="rotate"]').click();$('credential-action-confirm').click();return true})()`)
	wait(`$('credential-secret-dialog').open && $('credential-secret').value!==window.testFirstSecret`)
	eval(`(()=>{document.querySelector('[data-close="credential-secret-dialog"]').click();return true})()`)
	wait(`!$('credential-action-confirm').disabled`)
	eval(`(()=>{document.querySelector('[data-credential-action="revoke"]').click();$('credential-action-confirm').click();return true})()`)
	wait(`$('credential-list').textContent.includes('已撤销') && !document.querySelector('[data-credential-action="rotate"]')`)
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/docs/api")); err != nil {
		t.Fatal(err)
	}
	wait(`document.querySelectorAll('#api-reference>details').length>10`)
	if !eval(`document.querySelector('a[href="/#settings/credentials"]')!==null && !document.body.textContent.includes(window.testFirstSecret||'xm_secret_placeholder')`) {
		t.Fatal("docs link or privacy issue")
	}
	for _, width := range []int64{1280, 390} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 900)); err != nil {
			t.Fatal(err)
		}
		if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
			t.Fatal("docs overflow", width)
		}
	}
	eval(`(()=>{document.querySelector('a[href="/#settings/credentials"]').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated && !$('settings-credentials').hidden`)
}
