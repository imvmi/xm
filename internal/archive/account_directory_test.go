package archive

import "testing"

func TestAccountDirectoryUpgradeAndAtomicWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if err = s.SavePosts("alice", []Post{{ID: "1", Quote: &Post{ID: "2", Username: "quote"}}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"DROP TABLE archive_accounts", "UPDATE schema_meta SET version=23 WHERE id=1"} {
		if _, err = s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err = s.DB.QueryRow("SELECT target FROM archive_accounts").Scan(&name); err != nil || name != "alice" {
		t.Fatal("legacy archive missing", name, err)
	}
	if _, err = s.DB.Exec("CREATE TRIGGER reject_pending BEFORE INSERT ON notification_pending BEGIN SELECT RAISE(ABORT,'injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePosts("bob", []Post{{ID: "3"}}); err == nil {
		t.Fatal("expected page failure")
	}
	var count int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM archive_accounts").Scan(&count); err != nil || count != 1 {
		t.Fatal("failed page or quote leaked directory entry", count, err)
	}
	if _, err = s.DB.Exec("DROP TRIGGER reject_pending"); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePosts("bob", []Post{{ID: "3"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePosts("bob", []Post{{ID: "3"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM archive_accounts").Scan(&count); err != nil || count != 2 {
		t.Fatal("directory deduplication", count, err)
	}
}
