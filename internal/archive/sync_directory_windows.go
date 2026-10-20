package archive

import (
	"errors"
	"os"
)

// SyncDirectory validates the directory handle. Windows has no portable
// equivalent to POSIX directory fsync; callers still Sync writable files before
// renaming them. Directory metadata durability is provided by the filesystem.
func SyncDirectory(dir *os.File) error {
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("directory handle required")
	}
	return nil
}
