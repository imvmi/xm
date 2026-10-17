package web

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"xm/internal/archive"
)

func TestPrivateAvatarAndUserView(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	dir := filepath.Join(a.Config().DataDir, "avatars")
	if err := archive.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "42"), png, 0600); err != nil {
		t.Fatal(err)
	}
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/avatars/42", nil, "", false), 401)
	b := login(t, a)
	w := b.request("GET", "/avatars/42", nil, "", false)
	ok(t, w, 200)
	if w.Header().Get("Content-Type") != "image/png" || len(w.Body.Bytes()) != len(png) {
		t.Fatal("avatar response incorrect", w.Header(), w.Body.Len())
	}
	w = b.request("GET", "/api/users", nil, "", false)
	ok(t, w, 200)
	var users []UserView
	if err := json.Unmarshal(w.Body.Bytes(), &users); err != nil || len(users) != 1 || users[0].AvatarURL != "/avatars/42" {
		t.Fatal("avatar URL missing from user view", err, w.Body.String())
	}
	ok(t, b.request("GET", "/avatars/not-an-id", nil, "", false), 404)
}
