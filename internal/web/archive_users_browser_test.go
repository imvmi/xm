package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"xm/internal/archive"
)

func TestArchiveDirectoryLoadsBeforeStatisticsBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	for _, name := range []string{"alice", "bob"} {
		if err := a.store.SavePosts(name, []archive.Post{{ID: name + "1", Username: name, Text: "目录测试"}}); err != nil {
			t.Fatal(err)
		}
	}
	seedMonitor(t, a, "alice")
	if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0,last_error=?,last_finished=123 WHERE username='alice'", archive.AccountSuspendedMessage); err != nil {
		t.Fatal(err)
	}
	handler := a.Handler()
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/users/stats" || r.URL.Path == "/api/overview" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	alloc, stopAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
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
				t.Fatal("UI timed out:", js)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := chromedp.Do(browser, chromedp.Navigate(server.URL+"/#archive")); err != nil {
		t.Fatal(err)
	}
	wait(`!document.getElementById('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{$('username').value='admin';$('password').value=` + string(password) + `;document.querySelector('#login-form button[type=submit]').click();return true})()`)
	wait(`document.querySelectorAll('#user-list .user-card').length===2`)
	if !eval(`state.users.every(u=>u.stats_pending) && $('user-list').textContent.includes('—') && $('user-empty').hidden && !$('user-status').hidden`) {
		t.Fatal("directory must be usable before any statistics response")
	}
	wait(`document.querySelector('[data-account="alice"]').textContent.includes('上次采集：账号已被冻结')`)
	eval(`(()=>{document.querySelector('[data-account="alice"]').click();return true})()`)
	wait(`$('account-menu-dialog').open && $('account-menu-content').textContent.includes('待账号解冻后')`)
	if !eval(`$('account-menu-content').textContent.includes('记录于') && $('account-menu-content').querySelector('[data-op="identity"]')!==null`) {
		t.Fatal("missing attention details/actions")
	}
	eval(`(()=>{$('account-menu-dialog').close();$('monitor-filter').value='attention';$('monitor-filter').dispatchEvent(new Event('change'));return true})()`)
	wait(`document.querySelectorAll('#user-list .user-card').length===1`)
	eval(`(()=>{$('monitor-filter').value='all';$('monitor-filter').dispatchEvent(new Event('change'));return true})()`)
	wait(`document.querySelectorAll('#user-list .user-card').length===2`)
	// Account timelines must also be usable while statistics remain blocked.
	eval(`(()=>{location.hash='user/alice';return true})()`)
	wait(`state.view==='user' && state.postIDs.size===1 && !state.postsLoading`)
	unblock()
	wait(`state.users.length===1 && !state.users[0].stats_pending`)
	eval(`(()=>{location.hash='archive';return true})()`)
	wait(`document.querySelectorAll('#user-list .user-card').length===2 && state.users.every(u=>!u.stats_pending)`)
	if !eval(`$('user-list').textContent.includes('alice') && $('user-list').textContent.includes('bob')`) {
		t.Fatal("account detail replaced full directory")
	}
	// Diagnose a reused handle without asserting the original account was deleted.
	if _, err := a.store.DB.Exec("UPDATE monitors SET last_error=? WHERE username='alice'", archive.AccountIdentityConflictMessage+"（原 ID：42，当前 ID：99），已停止采集"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO target_identities(target,user_id,current_username) VALUES('alice','42','alice')"); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SaveCookies(archive.Cookies{AuthToken: "fixture", CSRF: "fixture"}); err != nil {
		t.Fatal(err)
	}
	eval(`(()=>{refreshMonitors();return true})()`)
	wait(`state.monitors.find(m=>m.username==='alice').attention?.code==='identity_conflict'`)
	eval(`(()=>{$('monitor-filter').value='identity_conflict';$('monitor-filter').dispatchEvent(new Event('change'));return true})()`)
	wait(`document.querySelectorAll('#user-list .account-card').length===1`)
	eval(`(()=>{document.querySelector('[data-account="alice"] [data-op="manage"]').click();return true})()`)
	wait(`$('account-menu-dialog').open && $('account-menu-content').textContent.includes('核对原账号的新用户名')`)
	if !eval(`$('account-menu-content').textContent.includes('这不能证明原账号已删除') && !!$('account-menu-content').querySelector('[data-op="recheck"]') && !!$('account-menu-content').querySelector('[data-op="archive-only"]') && !!$('account-menu-content').querySelector('a[href="https://x.com/i/user/42"]')`) {
		t.Fatal("missing takeover management")
	}
	for _, width := range []int64{1280, 390} {
		if err := chromedp.Do(browser, chromedp.EmulateViewport(width, 850)); err != nil {
			t.Fatal(err)
		}
		if !eval(`document.documentElement.scrollWidth<=innerWidth && $('account-menu-dialog').scrollWidth<=$('account-menu-dialog').clientWidth`) {
			t.Fatal("account handling dialog overflows")
		}
		if dir := os.Getenv("XM_UI_SCREENSHOT_DIR"); dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			img, err := chromedp.Run(browser, chromedp.FullScreenshot(90))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("account-takeover-%d.png", width)), img, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	eval(`(()=>{document.querySelector('#account-menu-content [data-op="recheck"]').click();return true})()`)
	wait(`!accountPending.size && !!state.monitors.find(m=>m.username==='alice').queued_job`)
	if !eval(`!state.monitors.find(m=>m.username==='alice').enabled`) {
		t.Fatal("manual recheck enabled monitoring")
	}
	// Cancelling the confirmation leaves the monitor and queued task intact.
	eval(`(()=>{window.confirm=()=>false;openAccountMenu('alice');document.querySelector('#account-menu-content [data-op="archive-only"]').click();return true})()`)
	if !eval(`state.monitors.some(m=>m.username==='alice'&&m.queued_job)`) {
		t.Fatal("cancelled action removed monitor")
	}
	eval(`(()=>{window.confirm=()=>true;openAccountMenu('alice');document.querySelector('#account-menu-content [data-op="archive-only"]').click();return true})()`)
	wait(`!accountPending.size && !state.monitors.some(m=>m.username==='alice')`)
	var originalID string
	if err := a.store.DB.QueryRow("SELECT user_id FROM target_identities WHERE target='alice'").Scan(&originalID); err != nil || originalID != "42" {
		t.Fatalf("original identity lost: %s %v", originalID, err)
	}
	var count int
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM target_posts WHERE target='alice'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("archive lost: %d %v", count, err)
	}
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM collection_jobs WHERE target='alice' AND finished_at=0").Scan(&count); err != nil || count != 0 {
		t.Fatalf("queue retained: %d %v", count, err)
	}
	eval(`(()=>{$('monitor-filter').value='archived';$('monitor-filter').dispatchEvent(new Event('change'));return true})()`)
	wait(`document.querySelectorAll('#user-list .account-card').length===2`)
	eval(`(()=>{openAccountMenu('alice');return true})()`)
	if !eval(`!!document.querySelector('#account-menu-content [data-op="readd"]')`) {
		t.Fatal("missing restore monitoring action")
	}
}
