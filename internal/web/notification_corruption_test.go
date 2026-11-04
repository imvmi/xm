package web

import (
	"context"
	"strings"
	"testing"

	"xm/internal/archive"
)

func TestNotifierQuarantinesMalformedPayload(t *testing.T) {
	a := testApp(t)
	ch := notificationFixture(t, a)
	tx, err := a.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, id := range []string{"bad", "good"} {
		if err = enqueueNotification(tx, []archive.NotificationChannel{ch}, NotificationEvent{ID: id, Type: "new_posts"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.DB.Exec("UPDATE notification_deliveries SET payload='{private-invalid' WHERE event_id='bad'"); err != nil {
		t.Fatal(err)
	}
	sent := []string{}
	a.notifier.send = func(_ context.Context, _ archive.Config, _ archive.NotificationChannel, e NotificationEvent) error {
		sent = append(sent, e.ID)
		return nil
	}
	if a.store.DB.Dialect == "sqlite" {
		if _, err = a.store.DB.Exec("CREATE TRIGGER reject_quarantine BEFORE UPDATE ON notification_deliveries WHEN NEW.status='failed' BEGIN SELECT RAISE(ABORT,'injected failure'); END"); err != nil {
			t.Fatal(err)
		}
		if _, err = a.notifier.step(context.Background()); err == nil {
			t.Fatal("quarantine write failure ignored")
		}
		if _, err = a.store.DB.Exec("DROP TRIGGER reject_quarantine"); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if worked, err := a.notifier.step(context.Background()); err != nil || !worked {
			t.Fatal(worked, err)
		}
	}
	if len(sent) != 1 || sent[0] != "good" {
		t.Fatal("valid notification blocked", sent)
	}
	var status, message string
	var finished int64
	if err = a.store.DB.QueryRow("SELECT status,last_error,finished_at FROM notification_deliveries WHERE event_id='bad'").Scan(&status, &message, &finished); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || finished == 0 || strings.Contains(message, "private-invalid") {
		t.Fatal("invalid diagnostic", status, message, finished)
	}
	if worked, err := a.notifier.step(context.Background()); err != nil || worked {
		t.Fatal("failed payload retried", worked, err)
	}
}
