package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestQuoteDisplayBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	post := archive.Post{ID: "96", Username: "alice", Text: "deepest quote"}
	for _, id := range []string{"97", "98", "99", "100"} {
		child := post
		post = archive.Post{ID: id, Username: "alice", Text: "post " + id, QuoteID: child.ID, Quote: &child}
	}
	if err := a.store.SavePosts("alice", []archive.Post{post}); err != nil {
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
				detail, _ := chromedp.Run(browser, chromedp.Evaluate[string](`document.getElementById('toast')?.textContent||document.body.innerText.slice(-1000)`))
				t.Fatal("quote display UI timed out", js, detail)
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
	wait(`state.authenticated && document.querySelectorAll('#posts .quote-card').length===4`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.view==='settings' && $('quote-display-depth').value==='4'`)
	eval(`(()=>{$('settings-tab-display').click();$('quote-display-depth').value='1';$('archive-display-form').requestSubmit();return true})()`)
	wait(`state.settings.quote_display_depth===1 && !document.querySelector('#archive-display-form button[type=submit]').disabled`)
	for _, view := range []string{"home", "user/alice"} {
		eval(`(()=>{location.hash='` + view + `';return true})()`)
		wait(`!state.postsLoading && document.querySelectorAll('#posts .quote-card').length===1 && !!document.querySelector('#posts .quote-collapsed [data-open-post="98"]')`)
	}
	eval(`(()=>{document.querySelector('#posts .quote-collapsed button').click();return true})()`)
	wait(`state.focusPost==='98' && !!document.querySelector('#posts [data-post-id="98"] .quote-card')`)
	if !eval(`document.querySelector('[data-post-id="98"]').querySelectorAll('.quote-card').length===1 && !!document.querySelector('[data-post-id="98"] .quote-collapsed [data-open-post="96"]')`) {
		t.Fatal("folded quote did not open a readable detail")
	}
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.view==='settings' && $('quote-display-depth').value==='1'`)
	eval(`(()=>{$('settings-tab-display').click();$('quote-display-depth').value='0';$('archive-display-form').requestSubmit();return true})()`)
	wait(`state.settings.quote_display_depth===0 && !document.querySelector('#archive-display-form button[type=submit]').disabled`)
	if err := chromedp.Do(browser, chromedp.EmulateViewport(390, 844)); err != nil {
		t.Fatal(err)
	}
	if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
		t.Fatal("settings overflow on mobile")
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#home")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && state.authenticated && state.settings.quote_display_depth===0 && !!document.querySelector('#posts .quote-collapsed')`)
	if !eval(`document.querySelectorAll('#posts .quote-card').length===0 && !!document.querySelector('#posts .quote-collapsed [data-open-post="99"]')`) {
		t.Fatal("zero depth not restored after reload")
	}
}
