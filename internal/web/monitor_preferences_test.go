package web

import (
	"encoding/json"
	"reflect"
	"testing"

	"xm/internal/archive"
)

func TestMonitorTimelinePreference(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	for _, input := range []map[string]any{
		{"username": "alice", "enabled": true},
		{"username": "bob", "enabled": false, "show_in_timeline": false},
	} {
		ok(t, b.request("POST", "/api/monitors", input, "http://localhost", true), 201)
	}
	check := func(name string, want bool) {
		t.Helper()
		w := b.request("GET", "/api/monitors", nil, "", false)
		ok(t, w, 200)
		var list []Monitor
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		for _, m := range list {
			if m.Username == name {
				if m.ShowInTimeline != want {
					t.Fatalf("%s visibility=%v", name, m.ShowInTimeline)
				}
				return
			}
		}
		t.Fatal("missing monitor", name)
	}
	check("alice", true)
	check("bob", false)
	// Older clients omitting the new field cannot reset an existing preference.
	ok(t, b.request("PUT", "/api/monitors/bob", map[string]any{"enabled": false}, "http://localhost", true), 200)
	check("bob", false)
	ok(t, b.request("PUT", "/api/monitors/alice", map[string]any{"enabled": true, "show_in_timeline": false}, "http://localhost", true), 200)
	check("alice", false)
	// A paused monitor can still have a queued manual task. Visibility must not
	// change enabled/requested/running, schedule, or historical progress.
	if _, err := a.store.DB.Exec("UPDATE monitors SET requested=1,running=1,next_run=123,catchup=1,baseline_status='backfilling' WHERE username='bob'"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO collection_jobs(target,source,status,queued_at,queue_order) VALUES('bob','manual','queued',1,1)"); err != nil {
		t.Fatal(err)
	}
	for _, shown := range []bool{true, true, false, false} {
		ok(t, b.request("PUT", "/api/monitors/bob/timeline", map[string]bool{"show_in_timeline": shown}, "http://localhost", true), 200)
		check("bob", shown)
	}
	// The edit dialog must also preserve a paused monitor's manual task.
	ok(t, b.request("PUT", "/api/monitors/bob", map[string]any{"enabled": false, "show_in_timeline": true}, "http://localhost", true), 200)
	check("bob", true)
	var enabled, requested, running, catchup bool
	var next int64
	var baseline, status string
	if err := a.store.DB.QueryRow("SELECT enabled,requested,running,next_run,catchup,baseline_status FROM monitors WHERE username='bob'").Scan(&enabled, &requested, &running, &next, &catchup, &baseline); err != nil {
		t.Fatal(err)
	}
	if enabled || !requested || !running || next != 123 || !catchup || baseline != "backfilling" {
		t.Fatal("display preference changed collection state")
	}
	if err := a.store.DB.QueryRow("SELECT status FROM collection_jobs WHERE target='bob'").Scan(&status); err != nil || status != "queued" {
		t.Fatalf("manual job changed: %s %v", status, err)
	}
	ok(t, b.request("PUT", "/api/monitors/bob/timeline", map[string]any{}, "http://localhost", true), 400)
	ok(t, b.request("PUT", "/api/monitors/bob/timeline", map[string]any{"show_in_timeline": "false"}, "http://localhost", true), 400)
	ok(t, b.request("PUT", "/api/monitors/missing/timeline", map[string]bool{"show_in_timeline": false}, "http://localhost", true), 404)
	ok(t, b.request("PUT", "/api/monitors/bob/timeline", map[string]bool{"show_in_timeline": true}, "http://localhost", false), 403)
}

func TestMonitorTimelineSchemaUpgrade(t *testing.T) {
	a := testApp(t)
	for _, q := range []string{
		"INSERT INTO monitors(username,enabled,next_run) VALUES('legacy',0,123)",
		"ALTER TABLE monitors DROP COLUMN show_in_timeline",
	} {
		if _, err := a.store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(a.store); err != nil {
		t.Fatal(err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || len(list) != 1 || !list[0].ShowInTimeline || list[0].Enabled || list[0].NextRun != 123 {
		t.Fatalf("upgrade: %+v %v", list, err)
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET show_in_timeline=0"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(a.store); err != nil {
		t.Fatal(err)
	}
	list, err = monitors(a.store.DB)
	if err != nil || len(list) != 1 || list[0].ShowInTimeline {
		t.Fatalf("upgrade reset preference: %+v %v", list, err)
	}
}

func TestHomeTimelineVisibility(t *testing.T) {
	a := testApp(t)
	for _, q := range []string{
		"INSERT INTO monitors(username,enabled,next_run,show_in_timeline) VALUES('hidden',1,1,0)",
		"INSERT INTO monitors(username,enabled,next_run) VALUES('visible',0,1)",
	} {
		if _, err := a.store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	quote := archive.Post{ID: "5", Username: "hidden", Text: "needle", Raw: json.RawMessage(`{}`), Assets: []archive.Asset{{ID: "photo", Kind: "photo", URL: "https://pbs.twimg.com/media/fixture.jpg"}}}
	shared := archive.Post{ID: "100", Username: "shared", Raw: json.RawMessage(`{}`), Quote: &quote}
	for target, posts := range map[string][]archive.Post{
		"hidden":      {{ID: "999", Username: "hidden", Raw: json.RawMessage(`{}`), Quote: &quote}, shared},
		"visible":     {shared, {ID: "9", Username: "visible", Raw: json.RawMessage(`{}`)}},
		"unmonitored": {{ID: "99", Username: "unmonitored", Raw: json.RawMessage(`{}`)}},
	} {
		if err := a.store.SavePosts(target, posts); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, target, query, kind, before string
		limit, total                      int
		ids                               []string
		more                              bool
	}{
		{"home", "", "", "", "", 10, 3, []string{"100", "99", "9"}, false},
		{"first", "", "", "", "", 1, 3, []string{"100"}, true},
		{"next", "", "", "", "100", 1, 3, []string{"99"}, true},
		{"last", "", "", "", "99", 1, 3, []string{"9"}, false},
		{"search", "", "needle", "", "", 10, 1, []string{"100"}, false},
		{"media", "", "", "photo", "", 10, 1, []string{"100"}, false},
		{"hidden archive", "hidden", "", "", "", 10, 2, []string{"999", "100"}, false},
		{"hidden archive search", "hidden", "needle", "photo", "", 10, 2, []string{"999", "100"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := listPostsPage(a.store.DB, tc.target, tc.query, tc.kind, "", 0, tc.limit, tc.before, true)
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, item := range p.Items {
				ids = append(ids, item.ID)
			}
			if p.Total != tc.total || p.HasMore != tc.more || !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("got total=%d ids=%v more=%v", p.Total, ids, p.HasMore)
			}
			if len(p.Items) > 0 && p.Items[0].ID == "100" && (p.Items[0].Quote == nil || p.Items[0].Quote.ID != "5") {
				t.Fatal("hidden author's embedded quote disappeared")
			}
		})
	}
	// Hiding every monitor leaves unmonitored archives available on home.
	if _, err := a.store.DB.Exec("UPDATE monitors SET show_in_timeline=0"); err != nil {
		t.Fatal(err)
	}
	p, err := listPostsPage(a.store.DB, "", "", "", "", 0, 24, "", false)
	if err != nil || p.Total != -1 || len(p.Items) != 1 || p.Items[0].ID != "99" {
		t.Fatalf("hidden page: %+v %v", p, err)
	}
	users, err := archiveUsers(a.store.DB)
	if err != nil || len(users) != 3 {
		t.Fatalf("account archive hidden: %+v %v", users, err)
	}
}
