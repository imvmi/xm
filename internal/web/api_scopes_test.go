package web

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"xm/internal/archive"
)

func TestAPILegacyScopeCompatibility(t *testing.T) {
	for _, test := range []struct {
		old, want []string
	}{
		{[]string{"accounts:write"}, []string{"accounts:create", "accounts:update"}},
		{[]string{"tasks:create"}, []string{"tasks:retry", "tasks:submit:backfill", "tasks:submit:incremental"}},
		{[]string{"tasks:create", "tasks:rescan"}, []string{"tasks:retry", "tasks:submit:backfill", "tasks:submit:incremental", "tasks:submit:rescan"}},
		{[]string{"tasks:rescan"}, []string{}},
		{[]string{"timeline:read"}, []string{"posts:read", "timeline:list"}},
		{[]string{"media:read"}, []string{"avatars:read", "media:download"}},
		{[]string{"accounts:write", "accounts:create"}, []string{"accounts:create", "accounts:update"}},
	} {
		if got := expandAPIScopes(test.old); !slices.Equal(got, test.want) {
			t.Errorf("expand %v = %v, want %v", test.old, got, test.want)
		}
	}

	a := testApp(t)
	c, secret := createTestCredential(t, a, fullTestCredential())
	// Simulate a persisted credential from before the split, without reissuing it.
	if _, err := a.store.DB.Exec("UPDATE api_credentials SET scopes=? WHERE id=?", `["accounts:write","tasks:create","tasks:rescan","timeline:read","media:read"]`, c.ID); err != nil {
		t.Fatal(err)
	}
	w := apiTestRequest(a, secret, "GET", "/api/v1/me", nil, "")
	ok(t, w, 200)
	var effective apiCredential
	if err := json.Unmarshal(w.Body.Bytes(), &effective); err != nil {
		t.Fatal(err)
	}
	if !effective.allows("accounts:create") || !effective.allows("accounts:update") || !effective.allows("tasks:submit:rescan") || effective.allows("accounts:write") {
		t.Fatal("legacy credential did not expose effective grants", effective.Scopes)
	}
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", map[string]string{"username": "alice"}, "legacy-create"), 201)
	in := fullTestCredential()
	in.Scopes = []string{"accounts:update"}
	b := login(t, a)
	ok(t, b.request("PATCH", "/api/credentials/"+c.ID, in, "http://localhost", true), 200)
	// Even a previously successful idempotent request loses access immediately.
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", map[string]string{"username": "alice"}, "legacy-create"), 403)
	ok(t, apiTestRequest(a, secret, "PATCH", "/api/v1/accounts/alice", map[string]bool{"enabled": false}, ""), 200)
	stored, err := a.apiCredential(c.ID)
	if err != nil || !slices.Equal(stored.Scopes, in.Scopes) {
		t.Fatal("saving narrowed scopes restored bundled permissions", stored.Scopes, err)
	}
	in.Scopes = []string{"accounts:*"}
	ok(t, b.request("PATCH", "/api/credentials/"+c.ID, in, "http://localhost", true), 400)
}

func TestAPISingleScopesDoNotGrantOtherOperations(t *testing.T) {
	a := testApp(t)
	for _, scope := range apiScopes {
		t.Run(scope, func(t *testing.T) {
			in := fullTestCredential()
			in.Scopes = []string{scope}
			_, secret := createTestCredential(t, a, in)
			for _, route := range externalRoutes {
				if route.Scope == "" || route.Scope == scope {
					continue
				}
				method, path, _ := strings.Cut(route.Pattern, " ")
				path = strings.NewReplacer("{account}", "alice", "{task}", "missing", "{post}", "100", "{asset}", "photo").Replace(path)
				w := apiTestRequest(a, secret, method, path, map[string]any{}, "denied")
				ok(t, w, 403)
				if !strings.Contains(w.Body.String(), "insufficient_scope") {
					t.Fatal("missing permission error", w.Body.String())
				}
			}
		})
	}
}

func TestAPIAccountCreateAndUpdateAreIndependent(t *testing.T) {
	a := testApp(t)
	in := fullTestCredential()
	in.Scopes = []string{"accounts:create"}
	_, secret := createTestCredential(t, a, in)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", map[string]string{"username": "alice"}, "create"), 201)
	ok(t, apiTestRequest(a, secret, "PATCH", "/api/v1/accounts/alice", map[string]bool{"enabled": true}, ""), 403)
	in.Scopes = []string{"accounts:update"}
	_, secret = createTestCredential(t, a, in)
	ok(t, apiTestRequest(a, secret, "PATCH", "/api/v1/accounts/alice", map[string]bool{"enabled": true}, ""), 200)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", map[string]string{"username": "alice"}, "create"), 403)
}

func TestAPITaskModesAreIndependent(t *testing.T) {
	for _, mode := range []string{"incremental", "backfill", "rescan"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			in := fullTestCredential()
			in.Scopes = []string{apiTaskScope(mode)}
			_, secret := createTestCredential(t, a, in)
			path := "/api/v1/accounts/alice/tasks"
			for _, other := range []string{"incremental", "backfill", "rescan", ""} {
				if other == mode || (other == "" && mode == "incremental") {
					continue
				}
				ok(t, apiTestRequest(a, secret, "POST", path, map[string]string{"mode": other}, "same-key"), 403)
			}
			assertCleanupCount(t, a, "collection_jobs", 0)
			assertCleanupCount(t, a, "api_idempotency", 0)
			requested := mode
			if mode == "incremental" {
				requested = "" // The default also requires incremental permission.
			}
			w := apiTestRequest(a, secret, "POST", path, map[string]string{"mode": requested}, "same-key")
			ok(t, w, 202)
			job := decodeExternalJob(t, w)
			if job.RequestedMode != mode {
				t.Fatal("wrong requested mode", job)
			}
			ok(t, apiTestRequest(a, secret, "POST", "/api/v1/tasks/"+job.ID+"/retry", nil, "retry"), 403)
		})
	}
}

func TestAPIRetryRequiresModeAndRechecksReplay(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	in := fullTestCredential()
	c, secret := createTestCredential(t, a, in)
	body := map[string]string{"mode": "rescan"}
	createPath := "/api/v1/accounts/alice/tasks"
	w := apiTestRequest(a, secret, "POST", createPath, body, "create")
	ok(t, w, 202)
	job := decodeExternalJob(t, w)
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/tasks/"+job.ID+"/cancel", nil, ""), 200)
	b := login(t, a)
	setScopes := func(scopes ...string) {
		t.Helper()
		in.Scopes = scopes
		ok(t, b.request("PATCH", "/api/credentials/"+c.ID, in, "http://localhost", true), 200)
	}
	retryPath := "/api/v1/tasks/" + job.ID + "/retry"
	for _, scopes := range [][]string{{"tasks:retry"}, {"tasks:submit:backfill"}, {"tasks:retry", "tasks:submit:rescan"}, {"tasks:retry", "tasks:submit:incremental"}} {
		setScopes(scopes...)
		ok(t, apiTestRequest(a, secret, "POST", retryPath, nil, "retry"), 403)
	}
	setScopes("tasks:retry", "tasks:submit:backfill")
	ok(t, apiTestRequest(a, secret, "POST", createPath, body, "create"), 403)
	w = apiTestRequest(a, secret, "POST", retryPath, nil, "retry")
	ok(t, w, 202)
	if decodeExternalJob(t, w).RequestedMode != "backfill" {
		t.Fatal("rescan retry must continue as backfill")
	}
	ok(t, apiTestRequest(a, secret, "POST", retryPath, nil, "retry"), 202)
	setScopes("tasks:retry", "tasks:submit:incremental")
	w = apiTestRequest(a, secret, "POST", retryPath, nil, "retry")
	ok(t, w, 403)
	if w.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("cached retry bypassed narrowed mode permission")
	}
	assertCleanupCount(t, a, "collection_jobs", 2)
}

func TestAPIArchiveReadScopesAreIndependent(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", Text: "authorized"}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ scope, path string }{
		{"timeline:list", "/api/v1/accounts/alice/timeline"},
		{"posts:read", "/api/v1/posts/100"},
	} {
		in := fullTestCredential()
		in.Scopes = []string{test.scope}
		_, secret := createTestCredential(t, a, in)
		ok(t, apiTestRequest(a, secret, "GET", test.path, nil, ""), 200)
	}
}

func TestAPIBinaryScopesAreIndependent(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "100", Assets: []archive.Asset{{ID: "photo", Kind: "photo"}}}}); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	for _, file := range []string{"avatars/42", "media/api.jpg"} {
		path := filepath.Join(a.Config().DataDir, file)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, png, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.store.DB.Exec("UPDATE media SET status='done',local_path='media/api.jpg',bytes=? WHERE post_id='100'", len(png)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ scope, path string }{
		{"media:download", "/api/v1/posts/100/media/photo"},
		{"avatars:read", "/api/v1/accounts/alice/avatar"},
	} {
		in := fullTestCredential()
		in.Scopes = []string{test.scope}
		_, secret := createTestCredential(t, a, in)
		w := apiTestRequest(a, secret, "GET", test.path, nil, "")
		ok(t, w, 200)
		if w.Body.String() != string(png) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("binary data or credential caching incorrect", test.scope, w.Header())
		}
	}
}
