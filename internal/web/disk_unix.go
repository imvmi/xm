//go:build !windows

package web

import "golang.org/x/sys/unix"

func availableDiskBytes(path string) (uint64, error) {
	var disk unix.Statfs_t
	if err := unix.Statfs(path, &disk); err != nil {
		return 0, err
	}
	return uint64(disk.Bavail) * uint64(disk.Bsize), nil
}
