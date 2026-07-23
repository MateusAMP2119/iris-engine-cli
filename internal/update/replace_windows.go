//go:build windows

package update

import (
	"fmt"
	"os"
)

// renameOver replaces path with tmp on Windows, where a running executable
// cannot be overwritten in place but CAN be renamed: the live binary moves
// aside to path+".old", tmp takes its place, and the aside file is removed
// best-effort (removal fails while the old binary is still running; the
// residue is deleted by the next update's pre-clean).
func renameOver(tmp, path string) error {
	old := path + ".old"
	_ = os.Remove(old)
	if err := os.Rename(path, old); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("move running executable aside: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Best-effort restore so a failed replace leaves the original in place.
		_ = os.Rename(old, path)
		return err
	}
	_ = os.Remove(old)
	return nil
}
