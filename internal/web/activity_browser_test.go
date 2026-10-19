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
	eval(`(()=>{document.querySelector('#job-queue [data-cancel-job]').click();return true})()`)
	wait(`document.getElementById('queue-count').textContent==='0' && !document.getElementById('job-queue').textContent.includes('@bob')`)
	var enabled bool
	if err = a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username='bob'").Scan(&enabled); err != nil || !enabled {
		t.Fatal("queue cancellation paused the monitor", enabled, err)
	}
}
