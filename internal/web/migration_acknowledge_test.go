package web

import (
	"net/http/httptest"
	"os"
	"testing"
)

func TestMigrationAcknowledgementPersistsAndGuards(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	path := "/api/migrations/acknowledge"
	input := map[string]string{"id": "completed-job"}
	unauth := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "http://localhost"+path, nil)
	request.Header.Set("Origin", "http://localhost")
	a.Handler().ServeHTTP(unauth, request)
	ok(t, unauth, 401)
	ok(t, b.request("POST", path, input, "http://localhost", false), 403)
	ok(t, b.request("POST", path, input, "https://other.example", true), 403)
	ok(t, b.request("POST", path, input, "http://localhost", true), 409)
	for _, status := range []string{"running", "switching", "paused", "failed"} {
		a.migrations.job = &migrationJob{ID: "completed-job", Kind: "media", Status: status}
		ok(t, b.request("POST", path, input, "http://localhost", true), 409)
	}
	a.migrations.job = &migrationJob{ID: "completed-job", Kind: "media", Source: "remote", Target: "local", Status: "completed", Done: 3, Total: 3, Message: "done"}
	ok(t, b.request("POST", path, map[string]string{"id": "older-job"}, "http://localhost", true), 409)
	ok(t, b.request("POST", path, map[string]string{}, "http://localhost", true), 409)
	a.migrations.running = true
	ok(t, b.request("POST", path, input, "http://localhost", true), 409)
	a.migrations.running = false
	for range 2 { // Retrying a successful acknowledgement is harmless.
		ok(t, b.request("POST", path, input, "http://localhost", true), 200)
	}
	reloaded, err := newMigrationManager(a)
	if err != nil {
		t.Fatal(err)
	}
	j := reloaded.snapshot()
	if j == nil || !j.Acknowledged || j.Status != "completed" || j.Done != 3 || j.Total != 3 || j.Message != "done" {
		t.Fatalf("completion record not preserved: %+v", j)
	}
	// A stale tab must not acknowledge a newer completed migration.
	a.migrations.job = &migrationJob{ID: "new-job", Status: "completed"}
	ok(t, b.request("POST", path, input, "http://localhost", true), 409)
	if a.migrations.snapshot().Acknowledged {
		t.Fatal("new job hidden by stale acknowledgement")
	}
}

func TestMigrationAcknowledgementWriteFailure(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	a.migrations.job = &migrationJob{ID: "job", Status: "completed", UpdatedAt: 123}
	// A directory at the journal path prevents its atomic replacement.
	if err := os.Mkdir(a.migrations.journal(), 0700); err != nil {
		t.Fatal(err)
	}
	ok(t, b.request("POST", "/api/migrations/acknowledge", map[string]string{"id": "job"}, "http://localhost", true), 500)
	if j := a.migrations.snapshot(); j.Acknowledged || j.UpdatedAt != 123 {
		t.Fatalf("failed persistence hid completion: %+v", j)
	}
}
