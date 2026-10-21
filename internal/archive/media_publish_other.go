//go:build !windows

package archive

import "os"

func publishMigrationFile(root *os.Root, temporary, destination string) error {
	return root.Link(temporary, destination)
}
