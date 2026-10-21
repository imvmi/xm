package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsMigrationRenameDoesNotReplace(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "conflict"}[conflict], func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err = root.MkdirAll("媒体", 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "媒体", ".tmp"), []byte("verified"), 0600); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if err = os.WriteFile(filepath.Join(dir, "媒体", "target.jpg"), []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = renameMigrationTemp(root, "媒体/.tmp", "媒体/target.jpg")
			if (err != nil) != conflict {
				t.Fatal("unexpected no-replace rename result", err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "媒体", "target.jpg"))
			want := "verified"
			if conflict {
				want = "existing"
			}
			if err != nil || string(data) != want {
				t.Fatal("incorrect destination", string(data), err)
			}
		})
	}
}
