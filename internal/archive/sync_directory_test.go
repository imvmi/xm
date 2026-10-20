package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncDirectoryAfterAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "state.json")
	if err := AtomicWrite(name, []byte("before")); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(name, []byte("after")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := SyncDirectory(f); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(name); err != nil || string(got) != "after" {
		t.Fatalf("replacement: %q, %v", got, err)
	}
	f.Close()
	if err := SyncDirectory(f); err == nil {
		t.Fatal("closed directory handle must report an error")
	}
}
