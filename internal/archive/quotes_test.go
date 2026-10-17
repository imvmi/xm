package archive

import (
	"encoding/json"
	"strings"
	"testing"
)

func quotedFixture(t *testing.T) Post {
	t.Helper()
	var v object
	if err := json.Unmarshal(fixture(t, "quoted-post.json"), &v); err != nil {
		t.Fatal(err)
	}
	p, err := parsePost(v)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestQuotedTextMediaStorageAndBackfill(t *testing.T) {
	p := quotedFixture(t)
	if p.QuoteID != "102" || p.Quote == nil || p.Quote.Quote == nil || p.Quote.Text != "引用全文 & image-search-needle" {
		t.Fatalf("quote tree missing: %+v", p)
	}
	if !strings.Contains(p.Quote.Assets[0].URL, "name=orig") || p.Quote.Quote.Assets[0].URL != "https://video.twimg.com/high.mp4" {
		t.Fatal("quote media selection failed")
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Simulate a pre-upgrade archive: the raw JSON has quotes, normalized rows do not.
	old := p
	old.Quote = nil
	if err = s.SavePosts("alice", []Post{old}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.BackfillQuotes(); err != nil {
			t.Fatal(err)
		}
	}
	var posts, targets int
	s.DB.QueryRow("SELECT COUNT(*) FROM posts").Scan(&posts)
	s.DB.QueryRow("SELECT COUNT(*) FROM target_posts").Scan(&targets)
	if posts != 3 || targets != 1 {
		t.Fatalf("quote became standalone target or was lost: %d %d", posts, targets)
	}
	jobs, err := s.MediaJobs("alice", false)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("quote downloads not in target job list: %v %d", err, len(jobs))
	}
	if err = s.MediaResult(jobs[0], "media/done.mp4", 4, "hash", nil); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePosts("alice", []Post{p}); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.MediaJobs("alice", false)
	if err != nil || len(jobs) != 1 {
		t.Fatal("completed quote media queued again", err)
	}
	other, err := s.MediaJobs("unrelated", false)
	if err != nil || len(other) != 0 {
		t.Fatal("quote media leaked into unrelated target")
	}
	// Failure on a nested quote rolls back the parent and its target relation.
	_, err = s.DB.Exec(`CREATE TRIGGER reject_quote BEFORE INSERT ON posts WHEN NEW.id='102' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	p.ID = "104"
	if err = s.SavePosts("alice", []Post{p}); err == nil {
		t.Fatal("expected transaction failure")
	}
	s.DB.QueryRow("SELECT COUNT(*) FROM posts WHERE id='104'").Scan(&posts)
	if posts != 0 {
		t.Fatal("parent survived failed quote transaction")
	}
}
func TestUnavailableQuotesAndDepth(t *testing.T) {
	var v object
	json.Unmarshal(fixture(t, "quoted-post.json"), &v)
	v["quoted_status_result"] = object{"result": object{"__typename": "TweetUnavailable"}}
	obj(v["legacy"])["quoted_status_id_str"] = "hidden"
	p, err := parsePost(v)
	if err != nil || p.Quote != nil || p.QuoteID != "hidden" {
		t.Fatal("unavailable quote should preserve parent and reference")
	}
	// A cyclic object must not cause unbounded recursion (raw marshal can fail safely).
	v["quoted_status_result"] = object{"result": v}
	p, err = parsePost(v)
	if err != nil || p.Quote != nil {
		t.Fatal("self quote was followed")
	}
}
