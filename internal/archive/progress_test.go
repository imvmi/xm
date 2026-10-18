package archive

import (
	"testing"
)

func TestAdvanceReachedAtUsesTimelineTailAndOnlyMovesBackward(t *testing.T) {
	posts := []Post{
		{CreatedAt: "Thu Oct 15 12:00:00 +0000 2026"},
		{CreatedAt: "Fri Oct 16 12:00:00 +0000 2026"},
	}
	if got := advanceReachedAt("", posts); got != "2026-10-16T12:00:00Z" {
		t.Fatal(got)
	}
	if got := advanceReachedAt("2026-10-14T12:00:00Z", posts); got != "2026-10-14T12:00:00Z" {
		t.Fatal("progress moved forward", got)
	}
}

func TestCollectionPageCommitsPostsCursorAndCounters(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.StartRun("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("INSERT INTO run_metrics(run_id,new_posts,posts_read) VALUES(?,0,0)", id); err != nil {
		t.Fatal(err)
	}
	progress := CollectionProgress{UserID: "42", Posts: TimelineProgress{Cursor: "page-1", Head: "100"}}
	page := Page{Posts: []Post{{ID: "100", AuthorID: "42", Username: "alice"}}, Progress: &progress}
	for i := 0; i < 2; i++ {
		if err = s.SaveCollectionPage("alice", id, page); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadCollectionProgress("alice", "42")
	if err != nil || got.Posts.Cursor != "page-1" || got.SavedAt == 0 {
		t.Fatal(got, err)
	}
	var added, read int
	if err = s.DB.QueryRow("SELECT new_posts,posts_read FROM run_metrics WHERE run_id=?", id).Scan(&added, &read); err != nil || added != 1 || read != 2 {
		t.Fatal(added, read, err)
	}
	if _, err = s.LoadCollectionProgress("alice", "43"); FailureKind(err) != "checkpoint" {
		t.Fatal("cross-user checkpoint accepted", err)
	}
	if _, err = s.DB.Exec(`CREATE TRIGGER reject_progress BEFORE UPDATE ON collection_progress BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	progress.Posts.Cursor = "page-2"
	page.Posts = []Post{{ID: "99", AuthorID: "42", Username: "alice"}}
	if err = s.SaveCollectionPage("alice", id, page); err == nil {
		t.Fatal("expected checkpoint persistence failure")
	}
	var count int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM posts WHERE id='99'").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial transaction committed", count, err)
	}
	got, err = s.LoadCollectionProgress("alice", "42")
	if err != nil || got.Posts.Cursor != "page-1" {
		t.Fatal("cursor advanced after failed save", got, err)
	}
	if err = s.DB.QueryRow("SELECT new_posts,posts_read FROM run_metrics WHERE run_id=?", id).Scan(&added, &read); err != nil || added != 1 || read != 2 {
		t.Fatal("metrics advanced after failed save", added, read, err)
	}
}

func TestParsePaginationCursor(t *testing.T) {
	for _, op := range []string{"UserTweets", "UserOriginalsTimeline", "UserRepliesTimeline"} {
		p, err := ParsePage(resumeFixtureBody(0, 2), op)
		if err != nil || p.NextCursor != "page-1" || p.End {
			t.Fatal(p, err)
		}
		p, err = ParsePage(resumeFixtureBody(1, 2), op)
		if err != nil || !p.End {
			t.Fatal(p, err)
		}
	}
}
