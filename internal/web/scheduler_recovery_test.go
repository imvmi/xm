package web

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"xm/internal/archive"
)

// The subprocess exits without closing the store or finalizing the run. This
// exercises durable state, not just cancellation or an in-memory retry.
func TestSchedulerCrashRecovery(t *testing.T) {
	if path := os.Getenv("XM_TEST_CRASH_CONFIG"); path != "" {
		cfg, err := archive.LoadBootstrap(path)
		if err != nil {
			t.Fatal(err)
		}
		store, err := archive.OpenConfiguredStore(cfg)
		if err != nil {
			t.Fatal(err)
		}
		a, err := New(path, cfg, store, false)
		if err != nil {
			t.Fatal(err)
		}
		a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
			if err := save(archive.Page{Posts: []archive.Post{
				{ID: "200", AuthorID: "42", Username: "alice"},
				{ID: "190", AuthorID: "42", Username: "alice", ReplyTo: "1"},
			}}); err != nil {
				t.Fatal(err)
			}
			if os.Getenv("XM_TEST_CRASH_STAGE") == "collect" {
				os.Exit(23)
			}
			return archive.Collection{Posts: 2, Stop: "known_boundary_reached"}, nil
		}
		a.scheduler.download = func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error) {
			os.Exit(23)
			return archive.DownloadSummary{}, nil
		}
		_, err = a.scheduler.step(context.Background())
		t.Fatalf("subprocess did not reach crash point: %v", err)
	}
	for _, tc := range []struct {
		name, stage string
		seeded      bool
	}{{"after_partial_page", "collect", true}, {"before_final_commit", "media", true}, {"first_run_without_anchors", "collect", false}} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			postAnchor, replyAnchor := "", ""
			if tc.seeded {
				postAnchor, replyAnchor = "100", "90"
				if err := a.store.SavePosts("alice", []archive.Post{
					{ID: postAnchor, AuthorID: "42", Username: "alice"},
					{ID: replyAnchor, AuthorID: "42", Username: "alice", ReplyTo: "1"},
				}); err != nil {
					t.Fatal(err)
				}
			}
			cfg, path := a.Config(), a.configPath
			if err := a.store.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSchedulerCrashRecovery$")
			cmd.Env = append(os.Environ(), "XM_TEST_CRASH_CONFIG="+path, "XM_TEST_CRASH_STAGE="+tc.stage)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
				t.Fatalf("unexpected child exit: %v\n%s", err, output)
			}
			store, err := archive.OpenConfiguredStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			a, err = New(path, cfg, store, false)
			if err != nil {
				t.Fatal(err)
			}
			stopped, stop := context.WithCancel(context.Background())
			stop()
			if err = a.Run(stopped); err != nil {
				t.Fatal(err)
			}
			list, err := monitors(store.DB)
			if err != nil || len(list) != 1 {
				t.Fatal(list, err)
			}
			m := list[0]
			if m.Running || !m.Catchup || m.CatchupPost != postAnchor || m.CatchupReply != replyAnchor || m.LastStatus != "interrupted" || m.LastSuccess != 0 {
				t.Fatalf("lost pre-run checkpoint: %+v", m)
			}
			known, err := store.KnownTargetPostIDs(context.Background(), "alice", []string{"200", "190"})
			if err != nil || len(known) != 2 {
				t.Fatal("committed page lost", known, err)
			}
			a.scheduler.download = func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error) {
				return archive.DownloadSummary{}, nil
			}
			a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, identity archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
				if !identity.Catchup || identity.CatchupPostAnchor != postAnchor || identity.CatchupReplyAnchor != replyAnchor {
					t.Fatalf("partial run became a false boundary: %+v", identity)
				}
				err := save(archive.Page{Posts: []archive.Post{{ID: "150", AuthorID: "42", Username: "alice"}}})
				return archive.Collection{Posts: 1, Stop: "known_boundary_reached"}, err
			}
			if err = a.scheduler.queue("alice"); err != nil {
				t.Fatal(err)
			}
			if worked, err := a.scheduler.step(context.Background()); !worked || err != nil {
				t.Fatal(worked, err)
			}
			list, err = monitors(store.DB)
			if err != nil || list[0].Catchup || list[0].CatchupPost != "" || list[0].CatchupReply != "" || list[0].LastSuccess == 0 || list[0].LastStatus != "bounded_complete" {
				t.Fatal("checkpoint not finalized", list, err)
			}
		})
	}
}

func TestSchedulerFinalizationRollsBackRunWithCheckpoint(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite failure injection")
	}
	seedMonitor(t, a, "alice")
	if _, err := a.store.DB.Exec(`CREATE TRIGGER reject_completion BEFORE UPDATE ON monitors WHEN OLD.catchup=1 AND NEW.catchup=0 BEGIN SELECT RAISE(ABORT, 'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		return archive.Collection{Stop: "known_boundary_reached"}, nil
	}
	if _, err := a.scheduler.step(context.Background()); err == nil {
		t.Fatal("completion failure was ignored")
	}
	var status string
	if err := a.store.DB.QueryRow("SELECT status FROM runs ORDER BY id DESC LIMIT 1").Scan(&status); err != nil || status != "running" {
		t.Fatal("run committed without checkpoint", status, err)
	}
	list, err := monitors(a.store.DB)
	if err != nil || !list[0].Catchup || list[0].LastSuccess != 0 {
		t.Fatal("checkpoint lost on rollback", list, err)
	}
}

func TestSchedulerDoesNotCollectWithoutDurableCheckpoint(t *testing.T) {
	a := testApp(t)
	if a.store.DB.Dialect != "sqlite" {
		t.Skip("SQLite failure injection")
	}
	seedMonitor(t, a, "alice")
	if _, err := a.store.DB.Exec(`CREATE TRIGGER reject_checkpoint BEFORE UPDATE ON monitors WHEN NEW.catchup=1 BEGIN SELECT RAISE(ABORT, 'injected checkpoint failure'); END`); err != nil {
		t.Fatal(err)
	}
	called := false
	a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, _ string, _ archive.TargetIdentity, _ int, _ bool, _ func(archive.Page) error) (archive.Collection, error) {
		called = true
		return archive.Collection{}, nil
	}
	if _, err := a.scheduler.step(context.Background()); err == nil || called {
		t.Fatal("collector ran without a durable checkpoint", called, err)
	}
}
