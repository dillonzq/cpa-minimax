//go:build unix

package plugin

import (
	"os"
	"syscall"
)

// openCleanupRecord avoids blocking on a FIFO, even if a regular file is
// replaced before open. Callers compare the opened descriptor with Lstat.
func openCleanupRecord(root *os.Root, name string, flag int) (*os.File, error) {
	return root.OpenFile(name, flag|syscall.O_NONBLOCK, 0)
}
