package web

import (
	"context"
	"encoding/json"
	"testing"
	"xm/internal/archive"
)

func TestMonitorRenamePreservesArchiveAndNotifications(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	notificationFixture(t, a)
	current, postID, authorID := "alice", "100", "42"
	calls := 0
	a.scheduler.collect = func(ctx context.Context, cfg archive.Config, c archive.Cookies, target string, identity archive.TargetIdentity, n int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
		if target != "alice" {
			t.Fatal("archive key changed")
		}
		if calls > 0 && identity.UserID != "42" {
			t.Fatal("identity not retained", identity)
		}
		calls++
		if err := save(archive.Page{Profile: &archive.Profile{ID: authorID, Username: current}}); err != nil {
			return archive.Collection{}, err
		}
		err := save(archive.Page{Posts: []archive.Post{{ID: postID, AuthorID: authorID, Username: current, Text: "PRIVATE", Raw: json.RawMessage(`{}`)}}})
		return archive.Collection{Posts: 1}, err
	}
	runCollection(t, a)
	current, postID = "newalice", "101"
	runCollection(t, a)
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 1 || list[0].Username != "alice" || list[0].CurrentUsername != "newalice" || list[0].Posts != 2 {
		t.Fatal(list, err)
	}
	users, err := archiveUsers(a.store.DB)
	if err != nil || len(users) != 1 || users[0].CurrentUsername != "newalice" || users[0].Posts != 2 {
		t.Fatal(users, err)
	}
	counts := map[string]int{}
	for _, e := range events(t, a) {
		counts[e.Type]++
	}
	if counts["username_changed"] != 1 || counts["new_posts"] != 1 {
		t.Fatal(counts)
	}
	runCollection(t, a)
	if len(events(t, a)) != 2 {
		t.Fatal("rename or historical posts notified twice")
	}
	b := login(t, a)
	ok(t, b.request("POST", "/api/monitors", Monitor{Username: "newalice", Interval: 60, Limit: 10, Enabled: true}, "http://localhost", true), 409)
	authorID, current, postID = "99", "alice", "102"
	runCollection(t, a)
	list, err = monitors(a.store.DB)
	if err != nil || list[0].Enabled || list[0].Posts != 2 || list[0].CurrentUsername != "newalice" {
		t.Fatal("identity mismatch did not pause safely", list, err)
	}
	last := events(t, a)
	if last[len(last)-1].Type != "identity" {
		t.Fatal("missing identity notification", last)
	}
}
