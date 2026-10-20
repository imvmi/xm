package archive

import "testing"

func TestBookmarkSchemaUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePosts("alice", []Post{{ID: "100", Text: "existing archive"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("DROP TABLE post_bookmarks"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var text string
	if err = s.DB.QueryRow("SELECT text FROM posts WHERE id='100'").Scan(&text); err != nil || text != "existing archive" {
		t.Fatal("upgrade changed archive", err)
	}
	if _, err = s.DB.Exec("INSERT INTO post_bookmarks(post_id,created_at) VALUES('100',123)"); err != nil {
		t.Fatal("upgrade did not create bookmark table", err)
	}
}
