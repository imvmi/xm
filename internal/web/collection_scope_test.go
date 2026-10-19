package web

import (
	"context"
	"testing"

	"xm/internal/archive"
)

func TestEnablingRepliesRebuildsHistoryAndQueuedMode(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "queued"}[queued], func(t *testing.T) {
			a := testApp(t)
			b := login(t, a)
			seedMonitor(t, a, "alice")
			if _, err := a.store.DB.Exec("UPDATE monitors SET baseline_status='visible_history_complete',posts_baseline_status='visible_history_complete',catchup=1"); err != nil {
				t.Fatal(err)
			}
			if err := a.store.SaveCollectionPage("alice", 0, archive.Page{Progress: &archive.CollectionProgress{UserID: "42", Replies: archive.TimelineProgress{Done: true}, Posts: archive.TimelineProgress{Done: true}}}); err != nil {
				t.Fatal(err)
			}
			if queued {
				if err := a.scheduler.queue("alice"); err != nil {
					t.Fatal(err)
				}
			}
			ok(t, b.request("PUT", "/api/monitors/alice", map[string]any{"replies": true, "enabled": true}, "http://localhost", true), 200)
			list, err := monitors(a.store.DB)
			if err != nil || list[0].BaselineStatus != "not_started" || list[0].RepliesBaseline != "not_started" || list[0].Catchup {
				t.Fatal("scope was not reset", list, err)
			}
			a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, id archive.TargetIdentity, _ int, replies bool, _ func(archive.Page) error) (archive.Collection, error) {
				if !replies || id.CollectionMode != collectionModeBackfill || id.Progress.Replies.Done || id.Progress.Posts.Done {
					t.Fatal("new replies scope used old boundary", replies, id)
				}
				return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
			}
			schedulerStep(t, a)
			list, err = monitors(a.store.DB)
			if err != nil || list[0].BaselineStatus != "visible_history_complete" || list[0].RepliesBaseline != "visible_history_complete" {
				t.Fatal(list, err)
			}
		})
	}
}

func TestRunningCollectionCannotOverwriteChangedReplyScope(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "retry"}[retry], func(t *testing.T) {
			a := testApp(t)
			b := login(t, a)
			seedMonitor(t, a, "alice")
			if _, err := a.store.DB.Exec("UPDATE monitors SET baseline_status='visible_history_complete',posts_baseline_status='visible_history_complete'"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, id archive.TargetIdentity, _ int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
				calls++
				if calls == 1 {
					ok(t, b.request("PUT", "/api/monitors/alice", map[string]any{"replies": true, "enabled": true}, "http://localhost", true), 200)
					// Simulate the old task saving a completed page after the edit committed.
					if err := save(archive.Page{Profile: &archive.Profile{ID: "42", Username: "alice"}, Progress: &archive.CollectionProgress{UserID: "42", Posts: archive.TimelineProgress{Done: true}}}); err != nil {
						t.Fatal(err)
					}
					if retry {
						return archive.Collection{}, &archive.Failure{Kind: "timeline", Message: "fixture"}
					}
				} else if !replies || id.CollectionMode != collectionModeBackfill || id.Progress.Posts.Done {
					t.Fatal("old task overwrote new scope", replies, id)
				}
				return archive.Collection{Stop: "known_boundary_reached", HistoryComplete: true}, nil
			}
			schedulerStep(t, a)
			list, err := monitors(a.store.DB)
			if err != nil || list[0].BaselineStatus != "not_started" || list[0].Catchup {
				t.Fatal(list, err)
			}
			if _, err = a.store.DB.Exec("UPDATE collection_jobs SET retry_at=0"); err != nil {
				t.Fatal(err)
			}
			makeDue(t, a)
			schedulerStep(t, a)
			if calls != 2 {
				t.Fatal(calls)
			}
		})
	}
}

func TestLegacyReplyScopeIsRepairedOnRestart(t *testing.T) {
	a := testApp(t)
	seedMonitor(t, a, "alice")
	if _, err := a.store.DB.Exec("UPDATE monitors SET replies=1,baseline_status='visible_history_complete',replies_baseline_status='not_started',catchup=1"); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SaveCollectionPage("alice", 0, archive.Page{Progress: &archive.CollectionProgress{UserID: "42", Replies: archive.TimelineProgress{Done: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := ensureCollectionModeSchema(a.store.DB); err != nil {
		t.Fatal(err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || list[0].BaselineStatus != "not_started" || list[0].Catchup {
		t.Fatal(list, err)
	}
	progress, err := a.store.LoadCollectionProgress("alice", "42")
	if err != nil || progress.Replies.Done {
		t.Fatal(progress, err)
	}
}
