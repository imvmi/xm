package archive

import (
	"errors"
	"os"
	"path"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func publishMigrationFile(root *os.Root, temporary, destination string) error {
	if err := root.Link(temporary, destination); err == nil || os.IsExist(err) {
		return err
	}
	// FAT/exFAT do not support hard links. A verified temporary file already
	// resides on the destination volume, so a no-replace rename can publish it.
	return renameMigrationTemp(root, temporary, destination)
}

func renameMigrationTemp(root *os.Root, temporary, destination string) error {
	if !safeObjectKey(temporary) || !safeObjectKey(destination) || path.Dir(temporary) != path.Dir(destination) {
		return errors.New("临时文件必须与目标位于同一目录")
	}
	dir, err := root.Open(path.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	// Resolve from the confined directory handle, rather than reconstructing
	// a potentially stale root path. Keep the handle open during the rename.
	buffer := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(dir.Fd()), &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return err
		}
		if n >= uint32(len(buffer)) {
			buffer = make([]uint16, n+1)
			continue
		}
		break
	}
	directory := windows.UTF16ToString(buffer)
	from, err := windows.UTF16PtrFromString(filepath.Join(directory, path.Base(temporary)))
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(filepath.Join(directory, path.Base(destination)))
	if err != nil {
		return err
	}
	// Deliberately omit MOVEFILE_REPLACE_EXISTING: a concurrently created target
	// is an error, never permission to overwrite its bytes.
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}
