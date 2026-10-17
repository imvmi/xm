package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xm/internal/archive"
)

func notificationFixture(t *testing.T, a *App) archive.NotificationChannel {
	t.Helper()
	ch := archive.NotificationChannel{ID: "tg", Name: "My Telegram", Kind: "telegram", Enabled: true, Updates: true, Alerts: true, BotToken: "123456:secret-token", ChatID: "-100123", ThreadID: 12}
	cfg := a.Config()
	cfg.NotificationChannels = []archive.NotificationChannel{ch}
	if e := a.saveConfig(cfg); e != nil {
		t.Fatal(e)
	}
	return ch
}
func events(t *testing.T, a *App) []NotificationEvent {
	t.Helper()
	rows, e := a.store.DB.Query("SELECT payload FROM notification_deliveries ORDER BY id")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var all []NotificationEvent
	for rows.Next() {
		var raw string
		var v NotificationEvent
		rows.Scan(&raw)
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			t.Fatal(e)
		}
		all = append(all, v)
	}
	return all
}
func runCollection(t *testing.T, a *App) {
	t.Helper()
	a.store.DB.Exec("UPDATE monitors SET next_run=0")
	worked, e := a.scheduler.step(context.Background())
	if e != nil || !worked {
		t.Fatal(worked, e)
	}
}
func TestNotificationNewPostsBaselineDedupAndPrivacy(t *testing.T) {
	a := testApp(t)
	notificationFixture(t, a)
	seedMonitor(t, a, "alice")
	id := "100"
	a.scheduler.collect = func(ctx context.Context, cfg archive.Config, c archive.Cookies, user string, identity archive.TargetIdentity, n int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
		p := archive.Post{ID: id, Username: user, Text: "PRIVATE SECRET", QuoteID: "quote", Quote: &archive.Post{ID: "quote", Text: "PRIVATE QUOTE"}}
		for range 2 {
			if e := save(archive.Page{Posts: []archive.Post{p}}); e != nil {
				return archive.Collection{}, e
			}
		}
		return archive.Collection{Posts: 1}, nil
	}
	runCollection(t, a)
	if len(events(t, a)) != 0 {
		t.Fatal("initial history notified")
	}
	id = "101"
	runCollection(t, a)
	all := events(t, a)
	if len(all) != 1 || all[0].Type != "new_posts" || all[0].Count != 1 {
		t.Fatal(all)
	}
	raw, _ := json.Marshal(all)
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("private body sent")
	}
	runCollection(t, a)
	if len(events(t, a)) != 1 {
		t.Fatal("duplicate poll notified")
	}
	// Saved posts survive a crash before notification creation and are detected on
	// the next run using the persisted seen set, even though no fresh insert occurs.
	a.store.SavePosts("alice", []archive.Post{{ID: "102", Username: "alice"}})
	runCollection(t, a)
	if all = events(t, a); len(all) != 2 || all[1].Count != 1 {
		t.Fatal("missed persisted unseen post", all)
	}
}
func TestNotificationIncidentsRecoveryAndStaleCookie(t *testing.T) {
	a := testApp(t)
	notificationFixture(t, a)
	seedMonitor(t, a, "alice")
	kind := "transient"
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		if kind == "" {
			return archive.Collection{}, nil
		}
		return archive.Collection{}, &archive.Failure{Kind: kind, Message: "raw-secret-never-send"}
	}
	runCollection(t, a)
	runCollection(t, a)
	if len(events(t, a)) != 0 {
		t.Fatal("transient error sent too early")
	}
	runCollection(t, a)
	runCollection(t, a)
	if all := events(t, a); len(all) != 1 || all[0].Type != "collection_failed" {
		t.Fatal(all)
	}
	kind = ""
	runCollection(t, a)
	if all := events(t, a); len(all) != 2 || all[1].Type != "recovered" {
		t.Fatal(all)
	}
	kind = "auth"
	runCollection(t, a)
	if all := events(t, a); len(all) != 3 || all[2].Type != "auth" {
		t.Fatal(all)
	}
	if worked, e := a.scheduler.step(context.Background()); worked || e != nil {
		t.Fatal("blocked scheduler ran", e)
	}
	setState(a.store.DB, "cookie_blocked", "")
	kind = ""
	runCollection(t, a)
	if all := events(t, a); len(all) != 4 || all[3].Type != "recovered" {
		t.Fatal(all)
	}
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		setState(a.store.DB, "cookie_imported", "replacement")
		return archive.Collection{}, &archive.Failure{Kind: "auth", Message: "stale"}
	}
	runCollection(t, a)
	if len(events(t, a)) != 4 {
		t.Fatal("stale cookie notification")
	}
}
func TestNotificationFiltersAndQueueRetries(t *testing.T) {
	a := testApp(t)
	ch := notificationFixture(t, a)
	cfg := a.Config()
	tx, _ := a.store.DB.Begin()
	e := NotificationEvent{ID: "stable-id", Type: "new_posts", Title: "New", Message: "2 posts", Count: 2}
	enqueueNotification(tx, cfg.NotificationChannels, e)
	enqueueNotification(tx, cfg.NotificationChannels, e)
	tx.Commit()
	if len(events(t, a)) != 1 {
		t.Fatal("duplicate outbox")
	}
	calls := 0
	a.notifier.send = func(context.Context, archive.Config, archive.NotificationChannel, NotificationEvent) error {
		calls++
		return &deliveryError{message: "rate limited", retryAfter: 120}
	}
	worked, err := a.notifier.step(context.Background())
	if !worked || err != nil {
		t.Fatal(worked, err)
	}
	var status string
	var attempts int
	var next int64
	a.store.DB.QueryRow("SELECT status,attempts,next_attempt FROM notification_deliveries").Scan(&status, &attempts, &next)
	if status != "pending" || attempts != 1 || next < time.Now().Unix()+119 {
		t.Fatal(status, attempts, next)
	}
	a.notifier.step(context.Background())
	if calls != 1 {
		t.Fatal("ignored retry delay")
	}
	// A replacement worker reads the durable pending delivery.
	a.notifier = newNotifier(a)
	a.notifier.send = func(context.Context, archive.Config, archive.NotificationChannel, NotificationEvent) error {
		return nil
	}
	a.store.DB.Exec("UPDATE notification_deliveries SET next_attempt=0")
	a.notifier.step(context.Background())
	a.store.DB.QueryRow("SELECT status FROM notification_deliveries").Scan(&status)
	if status != "sent" {
		t.Fatal(status)
	}
	tx, _ = a.store.DB.Begin()
	e.ID = "changed-target"
	enqueueNotification(tx, cfg.NotificationChannels, e)
	tx.Commit()
	cfg.NotificationChannels[0].ChatID = "different"
	a.saveConfig(cfg)
	a.notifier.send = func(context.Context, archive.Config, archive.NotificationChannel, NotificationEvent) error {
		t.Fatal("queued notice redirected")
		return nil
	}
	a.notifier.step(context.Background())
	a.store.DB.QueryRow("SELECT status FROM notification_deliveries WHERE event_id=?", e.ID).Scan(&status)
	if status != "cancelled" {
		t.Fatal(status)
	}
	ch.Updates = false
	tx, _ = a.store.DB.Begin()
	e.ID = "filtered"
	enqueueNotification(tx, []archive.NotificationChannel{ch}, e)
	tx.Commit()
	if len(events(t, a)) != 2 {
		t.Fatal("update filter ignored")
	}
}

type notificationTransport func(*http.Request) (*http.Response, error)

func (f notificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTelegramPayloadAndSafeErrors(t *testing.T) {
	ch := archive.NotificationChannel{Kind: "telegram", BotToken: "123:secret", ChatID: "-100", ThreadID: 9}
	event := NotificationEvent{ID: "e", Title: "XM", Message: "Notice"}
	c := &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.telegram.org/bot123:secret/sendMessage" || r.Method != "POST" || r.Header.Get("Cookie") != "" {
			t.Fatal("unsafe request")
		}
		var v map[string]any
		json.NewDecoder(r.Body).Decode(&v)
		if v["chat_id"] != "-100" || v["message_thread_id"] != float64(9) || v["text"] != "XM\n\nNotice" || v["protect_content"] != true || v["parse_mode"] != nil {
			t.Fatal(v)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})}
	if e := sendNotificationHTTP(context.Background(), c, ch, event); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		status    int
		body      string
		permanent bool
		retry     int
	}{{200, `{"ok":false,"error_code":400,"description":"secret"}`, true, 0}, {429, `{"ok":false,"error_code":429,"parameters":{"retry_after":180}}`, false, 180}, {200, `bad-secret-response`, false, 0}} {
		c.Transport = notificationTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})
		e := sendNotificationHTTP(context.Background(), c, ch, event)
		var de *deliveryError
		if !errors.As(e, &de) || de.permanent != tc.permanent || de.retryAfter != tc.retry || strings.Contains(e.Error(), "secret") {
			t.Fatal(e)
		}
	}
	c.Transport = notificationTransport(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("secret endpoint") })
	if e := sendNotificationHTTP(context.Background(), c, ch, event); e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal(e)
	}
}
func TestWebhookResponseAndRedirectSafety(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer destination.Close()
	redirect := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" || r.Header.Get("X-XM-Event-ID") != "event-id" {
			t.Error("missing webhook auth/id")
		}
		if redirect {
			http.Redirect(w, r, destination.URL, 307)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	ch := archive.NotificationChannel{Kind: "webhook", WebhookURL: server.URL, BearerToken: "private-token"}
	e := NotificationEvent{ID: "event-id", Title: "Test"}
	if err := sendNotificationHTTP(context.Background(), server.Client(), ch, e); err != nil {
		t.Fatal(err)
	}
	redirect = true
	if err := sendNotificationHTTP(context.Background(), server.Client(), ch, e); err == nil {
		t.Fatal("redirect accepted")
	}
}
func TestNotificationAPISecretsAndExistingCookieAlert(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	setState(a.store.DB, "cookie_blocked", "expired")
	ch := archive.NotificationChannel{Name: "Telegram", Kind: "telegram", BotToken: "123456:hidden-secret", ChatID: "100", Enabled: true, Alerts: true, Updates: true}
	w := b.request("POST", "/api/notifications/channels", ch, "http://localhost", true)
	ok(t, w, 200)
	var v map[string]string
	json.Unmarshal(w.Body.Bytes(), &v)
	ch.ID = v["id"]
	if len(events(t, a)) != 1 {
		t.Fatal("missed already blocked account")
	}
	ch.BotToken = ""
	ok(t, b.request("POST", "/api/notifications/channels", ch, "http://localhost", true), 200)
	if len(events(t, a)) != 1 {
		t.Fatal("save caused duplicate alert")
	}
	w = b.request("GET", "/api/notifications", nil, "", false)
	ok(t, w, 200)
	if strings.Contains(w.Body.String(), "hidden-secret") {
		t.Fatal("token leaked")
	}
	cfg, e := archive.LoadConfig(a.configPath)
	if e != nil || cfg.NotificationChannels[0].BotToken != "123456:hidden-secret" {
		t.Fatal("blank erased secret", e)
	}
	ok(t, b.request("POST", "/api/notifications/channels/"+ch.ID+"/test", nil, "http://localhost", false), 403)
	ok(t, b.request("POST", "/api/notifications/channels/"+ch.ID+"/test", nil, "http://localhost", true), 200)
	if all := events(t, a); len(all) != 2 || all[1].Type != "test" {
		t.Fatal(all)
	}
	ok(t, b.request("DELETE", "/api/notifications/channels/"+ch.ID, nil, "http://localhost", true), 200)
	a.notifier.send = func(context.Context, archive.Config, archive.NotificationChannel, NotificationEvent) error {
		t.Fatal("deleted channel used")
		return nil
	}
	a.notifier.step(context.Background())
	a.notifier.step(context.Background())
	anon := &browserClient{t: t, a: a}
	ok(t, anon.request("GET", "/api/notifications", nil, "", false), 401)
}

func TestNotificationRetryLimitAndManualRetry(t *testing.T) {
	a := testApp(t)
	notificationFixture(t, a)
	b := login(t, a)
	tx, _ := a.store.DB.Begin()
	enqueueNotification(tx, a.Config().NotificationChannels, NotificationEvent{Type: "test", Title: "Test"})
	tx.Commit()
	a.notifier.send = func(context.Context, archive.Config, archive.NotificationChannel, NotificationEvent) error {
		return &deliveryError{message: "temporary network failure"}
	}
	for range 5 {
		a.store.DB.Exec("UPDATE notification_deliveries SET next_attempt=0")
		if _, e := a.notifier.step(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	var status string
	var attempts int
	var id int64
	a.store.DB.QueryRow("SELECT id,status,attempts FROM notification_deliveries").Scan(&id, &status, &attempts)
	if status != "failed" || attempts != 5 {
		t.Fatal(status, attempts)
	}
	ok(t, b.request("POST", fmt.Sprintf("/api/notifications/deliveries/%d/retry", id), nil, "http://localhost", true), 200)
	a.notifier.send = func(context.Context, archive.Config, archive.NotificationChannel, NotificationEvent) error {
		return &deliveryError{message: "invalid token", permanent: true}
	}
	a.notifier.step(context.Background())
	a.store.DB.QueryRow("SELECT status,attempts FROM notification_deliveries").Scan(&status, &attempts)
	if status != "failed" || attempts != 1 {
		t.Fatal(status, attempts)
	}
}
func TestNotificationSenderDoesNotBlockCollection(t *testing.T) {
	a := testApp(t)
	notificationFixture(t, a)
	seedMonitor(t, a, "alice")
	tx, _ := a.store.DB.Begin()
	enqueueNotification(tx, a.Config().NotificationChannels, NotificationEvent{Type: "test", Title: "Test"})
	tx.Commit()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.notifier.send = func(context.Context, archive.Config, archive.NotificationChannel, NotificationEvent) error {
		close(started)
		<-release
		return nil
	}
	go func() { defer close(done); a.notifier.step(context.Background()) }()
	<-started
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		return archive.Collection{}, nil
	}
	runCollection(t, a)
	close(release)
	<-done
}
