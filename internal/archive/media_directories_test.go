package archive

import (
	"strings"
	"testing"
)

func TestMediaDirectoryIdentityAndQuotes(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := Post{ID: "100", AuthorID: "123", Username: "Alice", Assets: []Asset{{ID: "image", Kind: "photo"}}, Quote: &Post{ID: "101", AuthorID: "456", Username: "Bob", Assets: []Asset{{ID: "video", Kind: "video"}}}}
	if err = s.SavePosts("alice", []Post{p}); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.MediaJobs("alice", false)
	if err != nil || len(jobs) != 2 {
		t.Fatal(jobs, err)
	}
	for _, j := range jobs {
		name, err := s.MediaDirectory(j)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"123": "alice_123", "456": "bob_456"}[j.AuthorID]
		if want == "" || name != want {
			t.Fatalf("wrong quote ownership: %+v %s", j, name)
		}
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	name, err := s.MediaDirectory(MediaJob{AuthorID: "123", Username: "renamed"})
	if err != nil || name != "alice_123" {
		t.Fatal("rename split author's directory", name, err)
	}
	other, err := s.MediaDirectory(MediaJob{AuthorID: "789", Username: "Alice"})
	if err != nil || other == name {
		t.Fatal("reused handle merged different users", other, err)
	}
	for _, j := range []MediaJob{{AuthorID: "../../escape", Username: "../outside", PostID: "1"}, {Username: "", PostID: "2"}} {
		name, err := s.MediaDirectory(j)
		if err != nil || !mediaDirectoryPattern.MatchString(name) {
			t.Fatal("unsafe fallback directory", name, err)
		}
	}
	if _, err := mediaRelativePath(MediaJob{Directory: "../escape"}, ".jpg"); err == nil {
		t.Fatal("path traversal accepted")
	}
	a, _ := mediaRelativePath(MediaJob{Directory: name, PostID: "100", ID: "1"}, ".jpg")
	b, _ := mediaRelativePath(MediaJob{Directory: name, PostID: "101", ID: "1"}, ".jpg")
	if a == b || !strings.HasPrefix(a, "media/users/alice_123/") {
		t.Fatal("post filenames collide or escape author directory", a, b)
	}
}
