//go:build !windows

package remote

import "os"

// openShared opens name for reading; on Unix renaming or deleting an open
// file is always allowed.
func openShared(name string) (*os.File, error) { return os.Open(name) }
