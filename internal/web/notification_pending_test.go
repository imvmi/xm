package web

import (
	"fmt"
	"testing"

	"xm/internal/archive"
)

func TestNotificationPendingUpgradeAndOutboxRollback(t *testing.T) {
	a := testApp(t)
	notificationFixture(t, a)
	m := Monitor{Username: "alice", LastSuccess: 1}
	// Simulate an existing archive with one delivered post and two posts saved
	// before a crash/upgrade, including a CLI save without a scheduler run ID.
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "1"}, {ID: "2"}, {ID: "3"}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"DELETE FROM notification_pending",
		"INSERT INTO notification_targets VALUES('alice',1)",
		"INSERT INTO notification_seen VALUES('alice','1')",
	} {
		if _, err := a.store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.prepareNotifications(m); err != nil {
		t.Fatal(err)
	}
	assertCleanupCount(t, a, "notification_pending", 2)
	if err := a.prepareNotifications(m); err != nil {
		t.Fatal(err)
	}
	assertCleanupCount(t, a, "notification_pending", 2)
	if a.store.DB.Dialect == "sqlite" {
		if _, err := a.store.DB.Exec("CREATE TRIGGER reject_outbox BEFORE INSERT ON notification_deliveries BEGIN SELECT RAISE(ABORT,'injected outbox failure'); END"); err != nil {
			t.Fatal(err)
		}
		if err := a.recordNotifications(a.Config(), m, true, false, "", 0); err == nil {
			t.Fatal("outbox failure ignored")
		}
		assertCleanupCount(t, a, "notification_pending", 2)
		assertCleanupCount(t, a, "notification_seen", 1)
		if _, err := a.store.DB.Exec("DROP TRIGGER reject_outbox"); err != nil {
			t.Fatal(err)
		}
	}
	// Reopening the application must preserve both pending work and the marker.
	restarted, err := New(a.configPath, a.Config(), a.store, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.scheduler.closeSession()
	if err := restarted.prepareNotifications(m); err != nil {
		t.Fatal(err)
	}
	if err := restarted.recordNotifications(a.Config(), m, true, false, "", 0); err != nil {
		t.Fatal(err)
	}
	assertCleanupCount(t, a, "notification_pending", 0)
	all := events(t, a)
	if len(all) != 1 || all[0].Count != 2 {
		t.Fatal("upgrade missed or repeated posts", all)
	}
	if err := a.store.SavePosts("alice", []archive.Post{{ID: "2"}, {ID: "4", Quote: &archive.Post{ID: "quote"}}}); err != nil {
		t.Fatal(err)
	}
	assertCleanupCount(t, a, "notification_pending", 1)
	if err := a.recordNotifications(a.Config(), m, true, false, "", 0); err != nil {
		t.Fatal(err)
	}
	if all = events(t, a); len(all) != 2 || all[1].Count != 1 {
		t.Fatal("duplicate or quote notified", all)
	}
}

func TestNotificationPendingChunkingAndIsolation(t *testing.T) {
	a := testApp(t)
	notificationFixture(t, a)
	m := Monitor{Username: "alice", LastSuccess: 1}
	if err := a.prepareNotifications(m); err != nil {
		t.Fatal(err)
	}
	posts := make([]archive.Post, 1101)
	for i := range posts {
		posts[i] = archive.Post{ID: fmt.Sprint(i + 1)}
	}
	if err := a.store.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SavePosts("bob", posts[:1]); err != nil {
		t.Fatal(err)
	}
	if err := a.recordNotifications(a.Config(), m, true, false, "", 0); err != nil {
		t.Fatal(err)
	}
	assertCleanupCount(t, a, "notification_pending", 1)
	all := events(t, a)
	if len(all) != 1 || all[0].Count != len(posts) {
		t.Fatal("chunked notification count", all)
	}
	if err := a.recordNotifications(a.Config(), m, true, false, "", 0); err != nil {
		t.Fatal(err)
	}
	if len(events(t, a)) != 1 {
		t.Fatal("empty queue repeated notification")
	}
}
