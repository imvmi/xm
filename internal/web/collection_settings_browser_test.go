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

func TestCollectionSettingsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alloc, closeAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer closeAlloc()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	eval := func(js string) bool {
		t.Helper()
		result, err := chromedp.Run(browser, chromedp.Evaluate[bool](js))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	wait := func(js string) {
		t.Helper()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for !eval(js) {
			select {
			case <-ctx.Done():
				t.Fatal("UI timed out", js)
			case <-ticker.C:
			}
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`!document.getElementById('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{document.getElementById('username').value='admin';document.getElementById('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated`)
	eval(`(()=>{location.hash='settings';return true})()`)
	wait(`state.settings!==null && !document.getElementById('view-settings').hidden`)
	eval(`(()=>{document.getElementById('settings-tab-collection').click();return true})()`)
	if !eval(`!document.getElementById('settings-collection').hidden && !document.getElementById('request-interval') && !document.getElementById('max-posts-per-run') && !document.getElementById('page-budget') && !document.getElementById('max-scrolls') && document.getElementById('default-interval').value==='1' && document.getElementById('queue-interval').value==='0' && document.getElementById('scroll-delay').value==='3' && document.getElementById('scroll-jitter').value==='0' && document.getElementById('timeline-idle-seconds').value==='24' && document.getElementById('known-boundary-posts').value==='8' && !document.getElementById('request-budget-mode') && document.getElementById('rate-limit-mode').value==='balanced' && document.getElementById('request-timeout').value==='60' && document.getElementById('max-media-mib').value==='512' && document.getElementById('browser-max-age').value==='6' && document.getElementById('browser-max-tasks').value==='100' && !document.getElementById('monitor-default-form')`) {
		t.Fatal("settings defaults/location incorrect")
	}
	if !eval(`(()=>{document.getElementById('scroll-delay').value='0';document.getElementById('scroll-jitter').value='5';document.getElementById('default-interval').value='0.5';return !document.getElementById('scroll-delay').checkValidity() && !document.getElementById('scroll-jitter').checkValidity() && !document.getElementById('default-interval').checkValidity()})()`) {
		t.Fatal("minimums not enforced")
	}
	eval(`(()=>{document.getElementById('default-interval').value='2';document.getElementById('queue-interval').value='45';document.getElementById('scroll-delay').value='6';document.getElementById('scroll-delay').dispatchEvent(new Event('input'));document.getElementById('scroll-jitter').value='2';document.getElementById('scroll-jitter').dispatchEvent(new Event('input'));document.getElementById('timeline-idle-seconds').value='40';document.getElementById('known-boundary-posts').value='12';document.getElementById('collection-timeout').value='600';document.getElementById('rate-limit-mode').value='fast';document.getElementById('request-timeout').value='90';document.getElementById('max-media-mib').value='1024';document.getElementById('browser-max-age').value='12';document.getElementById('browser-max-tasks').value='250';document.getElementById('media-retry-initial').value='10';document.getElementById('media-retry-max').value='48';document.querySelector('#collection-settings-form button[type="submit"]').click();return true})()`)
	wait(`state.settings.default_interval_seconds===7200 && state.settings.queue_interval_seconds===45 && state.settings.scroll_delay_seconds===6 && state.settings.scroll_jitter_seconds===2 && state.settings.rate_limit_mode==='fast'`)
	if a.Config().DefaultInterval != 7200 || a.Config().QueueInterval != 45 || a.Config().MaxPostsPerRun != 1000 || a.Config().PageBudget != 15 || a.Config().MaxScrolls != 40 || a.Config().ScrollDelay != 6 || a.Config().ScrollJitter != 2 || a.Config().TimelineIdleSeconds != 40 || a.Config().KnownBoundaryPosts != 12 || a.Config().CollectionTimeout != 600 || a.Config().RequestBudgetMode != "balanced" || a.Config().RateLimitMode != "fast" || a.Config().RequestTimeout != 90 || a.Config().MaxMediaBytes != 1024<<20 || a.Config().BrowserMaxAge != 12*3600 || a.Config().BrowserMaxTasks != 250 || a.Config().MediaRetryInitial != 10*60 || a.Config().MediaRetryMax != 48*3600 {
		t.Fatal("UI did not persist settings")
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#settings")); err != nil {
		t.Fatal(err)
	}
	wait(`typeof state!=='undefined' && state.authenticated && !document.getElementById('request-interval') && !document.getElementById('max-posts-per-run') && document.getElementById('scroll-delay').value==='6' && document.getElementById('scroll-jitter').value==='2' && document.getElementById('scroll-spacing-help').textContent.includes('4～8 秒') && document.getElementById('default-interval').value==='2' && document.getElementById('queue-interval').value==='45' && document.getElementById('timeline-idle-seconds').value==='40' && document.getElementById('collection-timeout').value==='600' && !document.getElementById('request-budget-mode') && document.getElementById('browser-max-tasks').value==='250' && document.getElementById('media-retry-max').value==='48'`)
}
