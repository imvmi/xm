//go:build !windows

package archive

import "os"

// SyncDirectory persists directory entries after files have been synced.
func SyncDirectory(dir *os.File) error {
	return dir.Sync()
}
