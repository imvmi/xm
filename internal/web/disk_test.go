package web

import (
	"path/filepath"
	"testing"
)

func TestAvailableDiskBytes(t *testing.T) {
	dir := t.TempDir()
	if free, err := availableDiskBytes(dir); err != nil || free == 0 {
		t.Fatalf("available space: %d, %v", free, err)
	}
	if _, err := availableDiskBytes(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory must not report usable disk space")
	}
}
