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

func TestTaskCenterBrowser(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("offline Chrome UI fixture")
	}
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	current, err := ensureCollectionJob(a.store.DB, "alice", "manual")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := a.store.StartJobRun("alice", current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.DB.Exec("UPDATE collection_jobs SET status='running',started_at=?,attempts=1,run_id=? WHERE id=?", time.Now().Unix(), runID, current.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.DB.Exec("INSERT INTO run_metrics(run_id,new_posts,posts_read) VALUES(?,?,?)", runID, 2, 8); err != nil {
		t.Fatal(err)
	}
	waiting, err := ensureCollectionJob(a.store.DB, "bob", "automatic")
	if err != nil {
		t.Fatal(err)
	}
	if err = finishCollectionJob(a.store.DB, waiting.ID, 0, "waiting_rate", "X 请求限流冷却中", true); err != nil {
		t.Fatal(err)
	}
	if err = setState(a.store.DB, "cooldown_until", "4102444800"); err != nil {
		t.Fatal(err)
	}

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
		result, evalErr := chromedp.Run(browser, chromedp.Evaluate[bool](js))
		if evalErr != nil {
			t.Fatal(evalErr)
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
				t.Fatal("task center UI timed out", js)
			case <-ticker.C:
			}
		}
	}
	if err = chromedp.Do(browser, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	wait(`!document.getElementById('login-view').hidden`)
	password, _ := json.Marshal(testPassword)
	eval(`(()=>{document.getElementById('username').value='admin';document.getElementById('password').value=` + string(password) + `;document.querySelector('#login-form button[type="submit"]').click();return true})()`)
	wait(`typeof state!=='undefined' && state.authenticated`)
	eval(`(()=>{location.hash='activity';return true})()`)
	wait(`!document.getElementById('view-activity').hidden && document.getElementById('current-job').textContent.includes('@alice') && document.getElementById('job-queue').textContent.includes('@bob')`)
	if !eval(`document.getElementById('current-job').textContent.includes('已读取 8 条') && document.getElementById('job-queue').textContent.includes('限流冷却') && document.getElementById('queue-count').textContent==='1' && !document.getElementById('queue-wait').hidden`) {
		t.Fatal("task center did not render current, queue, and wait state")
	}
	if !eval(`!document.getElementById('runs').textContent.includes('@alice') && document.getElementById('runs').textContent.includes('还没有已结束的执行')`) {
		t.Fatal("current task duplicated in recent history")
	}
	// Simulate identity refresh while task records retain their original keys.
	for i, target := range []string{"alice", "bob"} {
		id := []string{"42", "43"}[i]
		if err = a.store.SaveTargetProfile(target, archive.Profile{ID: id, Username: target}); err != nil {
			t.Fatal(err)
		}
		if err = a.store.SaveTargetProfile(target, archive.Profile{ID: id, Username: "new" + target}); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"core": map[string]any{"user_results": map[string]any{"result": map[string]any{"core": map[string]string{"name": "昵称 " + target}}}}})
		if err = a.store.SavePosts(target, []archive.Post{{ID: id, AuthorID: id, Username: target, Raw: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	historyID, err := a.store.StartRun("bob")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.EndRun(historyID, "bounded_complete", 1, ""); err != nil {
		t.Fatal(err)
	}
	eval(`(()=>{$('refresh-runs').click();return true})()`)
	wait(`$('current-job').textContent.includes('@newalice') && $('job-queue').textContent.includes('@newbob') && $('runs').textContent.includes('@newbob')`)
	if !eval(`[['current-job','alice'],['job-queue','bob'],['runs','bob']].every(([id,key])=>{const root=$(id),name=root.querySelector('.task-archive-link'),handle=root.querySelector('.task-profile-link');return name.textContent==='昵称 '+key && name.getAttribute('href')==='#user/'+key && handle.textContent==='@new'+key && handle.href==='https://x.com/new'+key && handle.target==='_blank';}) && document.querySelector('#current-job [data-stop-target]').dataset.stopTarget==='alice'`) {
		t.Fatal("task nickname, current handle, or stable action key is incorrect")
	}
	if err = chromedp.Do(browser, chromedp.EmulateViewport(390, 844)); err != nil {
		t.Fatal(err)
	}
	if !eval(`document.documentElement.scrollWidth<=innerWidth`) {
		t.Fatal("task identity overflows mobile viewport")
	}
	eval(`(()=>{document.querySelector('#job-queue .task-archive-link').click();return true})()`)
	wait(`state.view==='user' && state.target==='bob' && $('profile-header').textContent.includes('@newbob')`)
	eval(`(()=>{location.hash='activity';return true})()`)
	wait(`state.view==='activity' && document.querySelector('#job-queue .task-profile-link')?.textContent==='@newbob'`)
	eval(`(()=>{document.querySelector('#job-queue [data-cancel-job]').click();return true})()`)
	wait(`document.getElementById('queue-count').textContent==='0' && !document.getElementById('job-queue').textContent.includes('@bob')`)
	var enabled bool
	if err = a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username='bob'").Scan(&enabled); err != nil || !enabled {
		t.Fatal("queue cancellation paused the monitor", enabled, err)
	}
	// Monitor page uses the same cancellation action and preserves monitoring.
	if err := a.scheduler.queue("bob"); err != nil {
		t.Fatal(err)
	}
	eval(`(()=>{location.hash='monitors';return true})()`)
	wait(`!!document.querySelector('[data-account="bob"]') && state.monitors.some(m=>m.username==='bob'&&m.queued_job)`)
	eval(`(()=>{document.querySelector('[data-account="bob"]').click();return true})()`)
	wait(`!!document.querySelector('[data-monitor="bob"][data-op="cancel-queue"]')`)
	eval(`(()=>{document.querySelector('[data-monitor="bob"][data-op="cancel-queue"]').click();return true})()`)
	wait(`!$('account-menu-dialog').open && !accountPending.size && state.monitors.some(m=>m.username==='bob'&&!m.queued_job)`)
	eval(`(()=>{document.querySelector('[data-account="bob"]').click();return true})()`)
	wait(`$('account-menu-dialog').open && !document.querySelector('#account-menu-content [data-op="cancel-queue"]')`)
	if err = a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username='bob'").Scan(&enabled); err != nil || !enabled {
		t.Fatal("monitor page cancellation paused account", enabled, err)
	}

}
