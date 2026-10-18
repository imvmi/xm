package web

import (
	"context"
	"testing"

	"xm/internal/archive"
)

func TestSchedulerResumesThenChecksNewHead(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	step := 0
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, identity archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
		step++
		switch step {
		case 1:
			err := save(archive.Page{Posts: []archive.Post{{ID: "500", AuthorID: "42", Username: "alice"}}, Progress: &archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Cursor: "next-page", Head: "500"}}})
			return archive.Collection{Posts: 1, Stop: "page_budget_reached"}, err
		case 2:
			if !identity.Catchup || identity.Progress.Posts.Cursor != "next-page" {
				t.Fatal("lost durable cursor", identity)
			}
			err := save(archive.Page{Posts: []archive.Post{{ID: "100", AuthorID: "42", Username: "alice"}}})
			return archive.Collection{Posts: 1, Stop: "known_boundary_reached", NeedsHeadRefresh: true, RefreshPostAnchor: "500"}, err
		case 3:
			if !identity.Catchup || identity.CatchupPostAnchor != "500" || identity.Progress.Posts.Cursor != "" {
				t.Fatal("new head window was skipped", identity)
			}
			err := save(archive.Page{Posts: []archive.Post{{ID: "600", AuthorID: "42", Username: "alice"}, {ID: "500", AuthorID: "42", Username: "alice"}}})
			return archive.Collection{Posts: 2, Stop: "known_boundary_reached"}, err
		}
		t.Fatal("unexpected run")
		return archive.Collection{}, nil
	}
	for i := 0; i < 3; i++ {
		if _, err := a.store.DB.Exec("UPDATE monitors SET next_run=0"); err != nil {
			t.Fatal(err)
		}
		if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
			t.Fatal(worked, err)
		}
		list, err := monitors(a.store.DB)
		if err != nil || list[0].Catchup != (i < 2) {
			t.Fatal(list, err)
		}
		// Restart the service between pages, retaining only durable database state.
		b, err := New(a.configPath, a.Config(), a.store, false)
		if err != nil {
			t.Fatal(err)
		}
		b.scheduler.collect = a.scheduler.collect
		b.scheduler.download = a.scheduler.download
		b.scheduler.avatar = a.scheduler.avatar
		a = b
	}
	runs, err := recentRuns(a.store.DB)
	if err != nil || len(runs) != 3 {
		t.Fatal(runs, err)
	}
	if runs[0]["posts"] != 2 || runs[0]["new_posts"] != int64(1) {
		t.Fatal("read and new counts conflated", runs[0])
	}
}

func TestCheckpointResetPreservesHistoryAndCooldown(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	p := archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Cursor: "expired"}}
	if err := a.store.SaveCollectionPage("alice", 0, archive.Page{Posts: []archive.Post{{ID: "100", AuthorID: "42", Username: "alice"}}, Progress: &p}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0,catchup=1,catchup_post_anchor='90',catchup_scroll_budget=160"); err != nil {
		t.Fatal(err)
	}
	if err := setState(a.store.DB, "cooldown_until", "9999999999"); err != nil {
		t.Fatal(err)
	}
	b := login(t, a)
	w := b.request("POST", "/api/monitors/alice/reset-progress", nil, "http://localhost", true)
	ok(t, w, 200)
	got, err := a.store.LoadCollectionProgress("alice", "42")
	if err != nil || got.Posts.Cursor != "" {
		t.Fatal(got, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || list[0].Posts != 1 || list[0].CatchupPost != "90" || !list[0].Requested || list[0].CatchupScrolls != 0 {
		t.Fatal(list, err)
	}
	if worked, err := a.scheduler.step(context.Background()); worked || err != nil {
		t.Fatal("reset bypassed global cooldown", worked, err)
	}
}

func TestCheckpointFailurePausesMonitor(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	a.scheduler.collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
		return archive.Collection{}, &archive.Failure{Kind: "checkpoint", Message: "请重置断点"}
	}
	if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
		t.Fatal(worked, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || list[0].Enabled || list[0].Requested {
		t.Fatal(list, err)
	}
}
