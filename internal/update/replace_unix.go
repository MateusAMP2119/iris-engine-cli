//go:build unix

package update

import "os"

// renameOver atomically renames tmp over path (rename within a directory is
// atomic on unix, and an open running executable may be replaced freely).
func renameOver(tmp, path string) error {
	return os.Rename(tmp, path)
}
