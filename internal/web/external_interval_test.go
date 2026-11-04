package web

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestExternalAccountIntervals(t *testing.T) {
	a := testApp(t)
	in := fullTestCredential()
	in.Accounts = []string{"alice", "bob", "invalid", "archive"}
	_, secret := createTestCredential(t, a, in)
	input := map[string]any{"username": "alice", "enabled": true, "include_replies": true, "interval_override_seconds": 300}
	w := apiTestRequest(a, secret, "POST", "/api/v1/accounts", input, "create-interval")
	ok(t, w, 201)
	var created externalAccountView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Interval != 300 || created.IntervalOverride != 300 {
		t.Fatal("creation response", created, err)
	}
	replay := apiTestRequest(a, secret, "POST", "/api/v1/accounts", input, "create-interval")
	ok(t, replay, 201)
	if replay.Body.String() != w.Body.String() {
		t.Fatal("idempotent response changed")
	}
	read := func(name string) externalAccountView {
		t.Helper()
		w := apiTestRequest(a, secret, "GET", "/api/v1/accounts/"+name, nil, "")
		ok(t, w, 200)
		var v externalAccountView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := time.Now().Unix()
	if _, err := a.store.DB.Exec("UPDATE monitors SET last_finished=?,baseline_status='visible_history_complete' WHERE username='alice'", before); err != nil {
		t.Fatal(err)
	}
	for _, interval := range []int{60, 604800, 0} {
		ok(t, apiTestRequest(a, secret, "PATCH", "/api/v1/accounts/alice", map[string]int{"interval_override_seconds": interval}, ""), 200)
		want := interval
		if want == 0 {
			want = a.Config().DefaultInterval
		}
		v := read("alice")
		if v.IntervalOverride != interval || v.Interval != want || !v.Enabled || !v.Replies || v.Baseline != "visible_history_complete" || v.NextRun < before+int64(want) {
			t.Fatal("interval-only patch changed other settings or missed scheduling", v)
		}
	}
	b := login(t, a)
	ok(t, b.request("PUT", "/api/settings", map[string]int{"default_interval_seconds": 7200}, "http://localhost", true), 200)
	if v := read("alice"); v.Interval != 7200 || v.IntervalOverride != 0 {
		t.Fatal("global inheritance", v)
	}
	ok(t, apiTestRequest(a, secret, "PATCH", "/api/v1/accounts/alice", map[string]int{"interval_override_seconds": 300}, ""), 200)
	ok(t, apiTestRequest(a, secret, "PATCH", "/api/v1/accounts/alice", map[string]bool{"enabled": false}, ""), 200)
	if v := read("alice"); v.IntervalOverride != 300 || v.Enabled || !v.Replies {
		t.Fatal("omitted interval changed", v)
	}
	for i, value := range []any{-1, 1, 59, 604801, 1.5, "300"} {
		ok(t, apiTestRequest(a, secret, "PATCH", "/api/v1/accounts/alice", map[string]any{"interval_override_seconds": value}, ""), 400)
		ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", map[string]any{"username": "invalid", "interval_override_seconds": value}, fmt.Sprint("invalid-", i)), 400)
	}
	if v := read("alice"); v.IntervalOverride != 300 {
		t.Fatal("invalid patch changed interval", v)
	}
	ok(t, apiTestRequest(a, secret, "POST", "/api/v1/accounts", map[string]string{"username": "bob"}, "default-interval"), 201)
	if v := read("bob"); v.IntervalOverride != 0 || v.Interval != 7200 {
		t.Fatal("default interval", v)
	}
	if err := a.store.SavePosts("archive", []archive.Post{{ID: "123", Username: "archive"}}); err != nil {
		t.Fatal(err)
	}
	if v := read("archive"); v.Monitored || v.Interval != 0 || v.IntervalOverride != 0 {
		t.Fatal("archive-only interval", v)
	}
	w = apiTestRequest(a, secret, "GET", "/api/v1/accounts", nil, "")
	ok(t, w, 200)
	var page struct {
		Items []externalAccountView `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 3 {
		t.Fatal(page, err)
	}
	for _, v := range page.Items {
		if v.ID == "alice" && v.IntervalOverride != 300 {
			t.Fatal("list interval", v)
		}
	}
	in.Scopes = []string{"accounts:read"}
	_, readOnly := createTestCredential(t, a, in)
	ok(t, apiTestRequest(a, readOnly, "PATCH", "/api/v1/accounts/alice", map[string]int{"interval_override_seconds": 60}, ""), 403)
	paths := externalOpenAPI()["paths"].(map[string]any)
	for path, method := range map[string]string{"/api/v1/accounts": "post", "/api/v1/accounts/{account}": "patch"} {
		op := paths[path].(map[string]any)[method].(map[string]any)
		body := op["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
		if body["properties"].(map[string]any)["interval_override_seconds"] == nil {
			t.Fatal("OpenAPI omitted interval", path)
		}
	}
}
