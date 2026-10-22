package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xm/internal/archive"
)

func createTestCredential(t *testing.T, a *App, in apiCredentialInput) (apiCredential, string) {
	t.Helper()
	b := login(t, a)
	w := b.request("POST", "/api/credentials", in, "http://localhost", true)
	ok(t, w, 201)
	var result struct {
		Credential apiCredential `json:"credential"`
		Secret     string        `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Secret == "" || strings.Contains(w.Body.String(), "secret_hash") {
		t.Fatal("credential disclosure contract", w.Body.String())
	}
	return result.Credential, result.Secret
}
func fullTestCredential() apiCredentialInput {
	return apiCredentialInput{Name: "test", Scopes: append([]string{}, apiScopes...), Accounts: []string{"alice"}, Enabled: true, RequestLimit: 600, TaskLimit: 60, QueueLimit: 10}
}
func apiTestRequest(a *App, secret, method, path string, input any, key string) *httptest.ResponseRecorder {
	raw := []byte{}
	if input != nil {
		raw, _ = json.Marshal(input)
	}
	r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+secret)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func decodeExternalJob(t *testing.T, w *httptest.ResponseRecorder) externalJobView {
	t.Helper()
	var j externalJobView
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	if j.ID == "" {
		t.Fatal("missing public task id", w.Body.String())
	}
	return j
}

func TestAPICredentialLifecycleAndAuthBoundary(t *testing.T) {
	a := testApp(t)
	in := fullTestCredential()
	c, secret := createTestCredential(t, a, in)
	b := login(t, a)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/me", nil, ""), 200)
	ok(t, apiTestRequest(a, secret, "GET", "/api/settings", nil, ""), 401)
	ok(t, b.request("GET", "/api/v1/me", nil, "", false), 401)
	w := b.request("GET", "/api/credentials", nil, "", false)
	ok(t, w, 200)
	if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), apiDigest(secret)) {
		t.Fatal("list leaked secret")
	}
	var hash string
	if err := a.store.DB.QueryRow("SELECT secret_hash FROM api_credentials WHERE id=?", c.ID).Scan(&hash); err != nil || hash != apiDigest(secret) {
		t.Fatal("secret not hashed", err)
	}
	ok(t, b.request("POST", "/api/credentials", in, "http://evil.example", true), 403)
	ok(t, b.request("POST", "/api/credentials", in, "http://localhost", false), 403)
	in.Enabled = false
	ok(t, b.request("PATCH", "/api/credentials/"+c.ID, in, "http://localhost", true), 200)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/me", nil, ""), 401)
	in.Enabled = true
	ok(t, b.request("PATCH", "/api/credentials/"+c.ID, in, "http://localhost", true), 200)
	w = b.request("POST", "/api/credentials/"+c.ID+"/rotate", nil, "http://localhost", true)
	ok(t, w, 200)
	var rotated map[string]string
	json.Unmarshal(w.Body.Bytes(), &rotated)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/me", nil, ""), 401)
	secret = rotated["secret"]
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/me", nil, ""), 200)
	if _, err := a.store.DB.Exec("UPDATE api_credentials SET expires_at=? WHERE id=?", time.Now().Unix()-1, c.ID); err != nil {
		t.Fatal(err)
	}
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/me", nil, ""), 401)
	ok(t, b.request("PATCH", "/api/credentials/"+c.ID, in, "http://localhost", true), 200)
	ok(t, b.request("DELETE", "/api/credentials/"+c.ID, nil, "http://localhost", true), 200)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/me", nil, ""), 401)
	w = b.request("GET", "/api/credentials/audit", nil, "", false)
	ok(t, w, 200)
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("audit leaked secret")
	}
}

func TestAPIAccountsAuthorizationAndIdempotency(t *testing.T) {
	a := testApp(t)
	_, secret := createTestCredential(t, a, fullTestCredential())
	input := map[string]any{"username": "alice", "enabled": false, "include_replies": true}
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", input, ""), 400)
	w := apiTestRequest(a, secret, "POST", "/api/v1/accounts", input, "create-alice")
	ok(t, w, 201)
	again := apiTestRequest(a, secret, "POST", "/api/v1/accounts", input, "create-alice")
	ok(t, again, 201)
	if again.Body.String() != w.Body.String() || again.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("creation not replayed")
	}
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", map[string]any{"username": "alice"}, "create-alice"), 409)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", map[string]any{"username": "bob"}, "create-bob"), 404)
	seedMonitor(t, a, "bob")
	w = apiTestRequest(a, secret, "GET", "/api/v1/accounts", nil, "")
	ok(t, w, 200)
	if strings.Contains(w.Body.String(), "bob") {
		t.Fatal("cross-account list leak")
	}
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/accounts/bob", nil, ""), 404)
	ok(t, apiTestRequest(a, secret, "PATCH", "/api/v1/accounts/alice", map[string]bool{"enabled": true}, ""), 200)
	var replies, enabled bool
	if err := a.store.DB.QueryRow("SELECT replies,enabled FROM monitors WHERE username='alice'").Scan(&replies, &enabled); err != nil || !replies || !enabled {
		t.Fatal("PATCH lost unspecified settings", err)
	}
	readonly := fullTestCredential()
	readonly.Scopes = []string{"accounts:read"}
	_, readSecret := createTestCredential(t, a, readonly)
	ok(t, apiTestRequest(a, readSecret, "DELETE", "/api/v1/accounts/alice/monitor", nil, ""), 403)
	ok(t, apiTestRequest(a, secret, "DELETE", "/api/v1/accounts/alice/monitor", nil, ""), 200)
}

func TestAPITasksModesOwnershipCancellationAndCleanup(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	_, secret := createTestCredential(t, a, fullTestCredential())
	w := apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "incremental"}, "task-1")
	ok(t, w, 202)
	j := decodeExternalJob(t, w)
	if j.Mode != "backfill" || j.RequestedMode != "incremental" {
		t.Fatal("mode upgrade not exposed", j)
	}
	again := apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "incremental"}, "task-1")
	ok(t, again, 202)
	if decodeExternalJob(t, again).ID != j.ID {
		t.Fatal("retry created duplicate task")
	}
	_, other := createTestCredential(t, a, fullTestCredential())
	ok(t, apiTestRequest(a, other, "GET", "/api/v1/tasks/"+j.ID, nil, ""), 404)
	ok(t, apiTestRequest(a, other, "POST", "/api/v1/tasks/"+j.ID+"/cancel", nil, ""), 404)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "rescan"}, "task-conflict"), 409)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/tasks/"+j.ID+"/cancel", nil, ""), 200)
	var enabled bool
	a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username='alice'").Scan(&enabled)
	if !enabled {
		t.Fatal("cancel disabled monitor")
	}
	w = apiTestRequest(a, secret, "POST", "/api/v1/tasks/"+j.ID+"/retry", map[string]any{}, "retry-1")
	ok(t, w, 202)
	retried := decodeExternalJob(t, w)
	if retried.ID == j.ID {
		t.Fatal("explicit retry reused public ID")
	}
	// Simulate a live task to exercise cancellation separately from pausing monitoring.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.scheduler.mu.Lock()
	a.scheduler.current = "alice"
	a.scheduler.cancel = cancel
	a.scheduler.mu.Unlock()
	if _, err := a.store.DB.Exec("UPDATE collection_jobs SET status='running' WHERE public_id=?", retried.ID); err != nil {
		t.Fatal(err)
	}
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/tasks/"+retried.ID+"/cancel", nil, ""), 202)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("running task was not cancelled")
	}
	a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username='alice'").Scan(&enabled)
	if !enabled {
		t.Fatal("running cancel disabled monitoring")
	}
	a.scheduler.mu.Lock()
	a.scheduler.current = ""
	a.scheduler.cancel = nil
	a.scheduler.mu.Unlock()
	if _, err := a.store.DB.Exec("UPDATE collection_jobs SET status='failed',finished_at=1"); err != nil {
		t.Fatal(err)
	}
	b := login(t, a)
	ok(t, b.request("POST", "/api/settings/cleanup", dataCleanupInput{Tasks: true, Confirm: true}, "http://localhost", true), 200)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/tasks/"+j.ID, nil, ""), 404)
	w = apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "rescan"}, "task-new")
	ok(t, w, 202)
	if decodeExternalJob(t, w).ID == j.ID {
		t.Fatal("public ID reused after history cleanup")
	}
}

func TestAPITimelineQuoteAndMediaBoundaries(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", Text: "authorized", QuoteID: "90", Quote: &archive.Post{ID: "90", Text: "quoted"}, Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("bob", []archive.Post{{ID: "200", Text: "PRIVATE BOB", Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}}); err != nil {
		t.Fatal(err)
	}
	_, secret := createTestCredential(t, a, fullTestCredential())
	w := apiTestRequest(a, secret, "GET", "/api/v1/accounts/alice/timeline?limit=1", nil, "")
	ok(t, w, 200)
	if !strings.Contains(w.Body.String(), "authorized") || !strings.Contains(w.Body.String(), "quoted") || strings.Contains(w.Body.String(), "PRIVATE BOB") {
		t.Fatal(w.Body.String())
	}
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/posts/90", nil, ""), 200)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/posts/200", nil, ""), 404)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/posts/200/media/photo", nil, ""), 404)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/accounts/bob/timeline", nil, ""), 404)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/accounts/alice/timeline?limit=101", nil, ""), 400)
	dir := filepath.Join(a.Config().DataDir, "media")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "api.jpg"), []byte("0123456789"), 0600)
	if _, err := a.store.DB.Exec("UPDATE media SET status='done',local_path='media/api.jpg',bytes=10,sha256=? WHERE post_id='100'", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v1/posts/100/media/photo?v="+strings.Repeat("a", 64), nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("Range", "bytes=1-3")
	out := httptest.NewRecorder()
	a.Handler().ServeHTTP(out, r)
	ok(t, out, 206)
	if out.Body.String() != "123" || out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("range or credential caching incorrect", out.Body.String(), out.Header())
	}
}

func TestAPILimitsMaintenanceAndPendingIdempotency(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	in := fullTestCredential()
	in.RequestLimit = 2
	in.TaskLimit = 1
	c, secret := createTestCredential(t, a, in)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/me", nil, ""), 200)
	ok(t, apiTestRequest(a, secret, "GET", "/api/v1/me", nil, ""), 200)
	w := apiTestRequest(a, secret, "GET", "/api/v1/me", nil, "")
	ok(t, w, 429)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry header")
	}
	a.api.limitMu.Lock()
	clear(a.api.windows)
	a.api.limitMu.Unlock()
	a.maintenance.Store(true)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "incremental"}, "blocked"), 503)
	a.maintenance.Store(false)
	a.api.limitMu.Lock()
	clear(a.api.windows)
	a.api.limitMu.Unlock()
	body := map[string]string{"mode": "incremental"}
	raw, _ := json.Marshal(body)
	path := "/api/v1/accounts/alice/tasks"
	if _, err := a.store.DB.Exec("INSERT INTO api_idempotency VALUES(?,?,?,0,'',?)", apiDigest(c.ID+"\x00pending"), c.ID, apiDigest("POST\x00"+path+"\x00"+string(raw)), time.Now().Unix()+86400); err != nil {
		t.Fatal(err)
	}
	w = apiTestRequest(a, secret, "POST", path, body, "pending")
	ok(t, w, 409)
	if !strings.Contains(w.Body.String(), "idempotency_pending") {
		t.Fatal(w.Body.String())
	}
	var jobs int
	a.store.DB.QueryRow("SELECT COUNT(*) FROM collection_jobs").Scan(&jobs)
	if jobs != 0 {
		t.Fatal("uncertain request submitted duplicate work")
	}
}

func TestAPIDocsAndMigrationPersistence(t *testing.T) {
	a := testApp(t)
	c, secret := createTestCredential(t, a, fullTestCredential())
	for _, path := range []string{"/docs/api", "/docs/api/openapi.json"} {
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		ok(t, w, 200)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("docs contain secret")
		}
	}
	spec := externalOpenAPI()
	paths := spec["paths"].(map[string]any)
	for _, route := range externalRoutes {
		method, path, _ := strings.Cut(route.Pattern, " ")
		if paths[path].(map[string]any)[strings.ToLower(method)] == nil {
			t.Fatal("undocumented route", route.Pattern)
		}
	}
	dst, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		t.Fatal(err)
	}
	if err = a.store.CopyDatabase(context.Background(), dst, nil); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err = dst.DB.QueryRow("SELECT secret_hash FROM api_credentials WHERE id=?", c.ID).Scan(&hash); err != nil || hash != apiDigest(secret) {
		t.Fatal("migration lost credential", err)
	}
}

func TestAPITaskResumesWithAutomaticCollectionPaused(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	c, secret := createTestCredential(t, a, fullTestCredential())
	w := apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "backfill"}, "resume")
	ok(t, w, 202)
	job := decodeExternalJob(t, w)
	// Recovering an API task must retain manual semantics even when a previous
	// attempt has cleared requested and automatic monitoring is disabled.
	if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0,requested=0 WHERE username='alice'"); err != nil {
		t.Fatal(err)
	}
	a.configMu.Lock()
	a.config.PollingPaused = true
	a.configMu.Unlock()
	a.scheduler = newScheduler(a)
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
	}
	a.scheduler.download = func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error) {
		return archive.DownloadSummary{}, nil
	}
	a.scheduler.avatar = func(context.Context, archive.Config, archive.Profile) error { return nil }
	schedulerStep(t, a)
	actual, err := a.externalJob(c, job.ID)
	if err != nil || actual.Status != "completed" {
		t.Fatal("paused automatic collection blocked API job", actual, err)
	}
}

func TestAPIConcurrentIdempotencyAndQuota(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	seedMonitor(t, a, "bob")
	in := fullTestCredential()
	in.Accounts = []string{"alice", "bob"}
	in.QueueLimit = 1
	_, secret := createTestCredential(t, a, in)
	replies := make(chan *httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies <- apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "incremental"}, "concurrent")
		}()
	}
	wg.Wait()
	close(replies)
	id := ""
	for w := range replies {
		ok(t, w, 202)
		j := decodeExternalJob(t, w)
		if id != "" && id != j.ID {
			t.Fatal("duplicate concurrent job")
		}
		id = j.ID
	}
	assertCleanupCount(t, a, "collection_jobs", 1)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts/bob/tasks", map[string]string{"mode": "backfill"}, "queue-limit"), 429)
}

func TestAPIRescanNeedsExtraPermission(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	in := fullTestCredential()
	in.Scopes = []string{"tasks:create"}
	_, secret := createTestCredential(t, a, in)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "rescan"}, "rescan-denied"), 403)
	assertCleanupCount(t, a, "collection_jobs", 0)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts/alice/tasks", map[string]string{"mode": "backfill"}, "backfill-allowed"), 202)
}
