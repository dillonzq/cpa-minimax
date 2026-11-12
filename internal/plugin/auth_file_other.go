//go:build !unix

package plugin

import "os"

// openCleanupRecord opens an entry for descriptor-based type checks.
// Non-Unix platforms do not need the Unix FIFO nonblocking-open flag.
func openCleanupRecord(root *os.Root, name string, flag int) (*os.File, error) {
	return root.OpenFile(name, flag, 0)
}
