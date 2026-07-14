package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSizeRotatorRecoversFromRotationFailure proves the size rotator never panics
// when a rotation fails partway through a transient filesystem error: the write
// that triggers the failing rotation returns the error, a subsequent write while
// the obstruction persists still returns an error rather than dereferencing the
// closed active file, and once the obstruction clears the rotator recovers and
// rotates on the next write. This pins the crash path a naive rotator hits --
// nulling the active file on close, then re-entering rotate() and closing a nil
// file on the very next log line after a transient disk error.
func TestSizeRotatorRecoversFromRotationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	rot, err := NewSizeRotator(path, 100, 3)
	if err != nil {
		t.Fatalf("NewSizeRotator: %v", err)
	}
	defer func() { _ = rot.Close() }()

	line := []byte(strings.Repeat("A", 49) + "\n") // 50 bytes each

	// Fill to the threshold so the next write must rotate.
	mustWrite(t, rot, line) // 50
	mustWrite(t, rot, line) // 100

	// Inject a transient rename failure: the rotation the next write triggers fails
	// partway, after the active file has been closed.
	injected := errors.New("injected rename failure")
	rot.rename = func(string, string) error { return injected }

	// The write that triggers the failing rotation returns the error, never panics.
	if _, err := rot.Write(line); err == nil {
		t.Fatal("write during a failing rotation returned nil, want the rotation error")
	}
	// The next write re-enters rotation while the obstruction persists -- the exact
	// path that used to close a nil active file and panic. It must return an error.
	if _, err := rot.Write(line); err == nil {
		t.Fatal("second write during a failing rotation returned nil, want an error (it must not panic)")
	}

	// Clear the obstruction: the rotator recovers and the next write succeeds and
	// rotates a backup into place.
	rot.rename = os.Rename
	if _, err := rot.Write(line); err != nil {
		t.Fatalf("write after the obstruction cleared did not recover: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected a rotated backup after recovery: %v", err)
	}
}

// mustWrite writes p to rot, failing the test on error.
func mustWrite(t *testing.T, rot *SizeRotator, p []byte) {
	t.Helper()
	if _, err := rot.Write(p); err != nil {
		t.Fatalf("write: %v", err)
	}
}
