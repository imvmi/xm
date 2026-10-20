package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"

	"xm/internal/archive"
)

// Exercise Serve's real listener/worker shutdown boundary, then reopen the new
// bootstrap as the command's restart loop does. Only loopback test schemas.
func TestMigrationServeRestart(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("isolated loopback MySQL required")
	}
	a := testApp(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- Serve(ctx, a.configPath, a.Config(), a.store, address, false) }()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + address
	ready := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			r, e := client.Get(base + "/healthz")
			if e == nil {
				r.Body.Close()
				if r.StatusCode == 200 {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("server did not become healthy")
	}
	ready()
	post := func(path, csrf string, input any) *http.Response {
		t.Helper()
		body, _ := json.Marshal(input)
		r, _ := http.NewRequest("POST", base+path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", base)
		r.Header.Set("X-CSRF-Token", csrf)
		response, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		return response
	}
	r := post("/api/login", "", map[string]string{"username": "admin", "password": testPassword})
	if r.StatusCode != 200 {
		r.Body.Close()
		t.Fatal("login failed", r.StatusCode)
	}
	r.Body.Close()
	r, err = client.Get(base + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	var session map[string]string
	err = json.NewDecoder(r.Body).Decode(&session)
	r.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	r = post("/api/migrations", session["csrf"], map[string]string{"kind": "database", "confirm": "开始迁移"})
	r.Body.Close()
	if r.StatusCode != 202 {
		t.Fatal("start failed", r.StatusCode)
	}
	select {
	case err = <-result:
		if !errors.Is(err, ErrMigrationRestart) {
			t.Fatal("unexpected shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cutover did not shut down old workers")
	}
	cfg, err := archive.LoadConfig(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	go func() { result <- Serve(ctx, a.configPath, cfg, dst, address, false) }()
	ready()
	r, err = client.Get(base + "/api/migrations")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal("old session accepted after restart", r.StatusCode)
	}
	r = post("/api/login", "", map[string]string{"username": "admin", "password": testPassword})
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("migrated credentials unusable", r.StatusCode)
	}
	r, err = client.Get(base + "/api/migrations")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Job    migrationJob `json:"job"`
		Driver string       `json:"database_driver"`
	}
	err = json.NewDecoder(r.Body).Decode(&status)
	r.Body.Close()
	if err != nil || status.Job.Status != "completed" || status.Driver != "sqlite" {
		t.Fatal("restart did not reconcile migration", err, status)
	}
	cancel()
	select {
	case err = <-result:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown timed out")
	}
}
