package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSymlinkPointsTo(t *testing.T) {
	want := filepath.Join(t.TempDir(), "iris-bin", "iris")
	linkDir := filepath.Join(t.TempDir(), "shimdir")
	cases := []struct {
		dest    string
		linkDir string
		ok      bool
	}{
		{want, linkDir, true},
		{filepath.Clean(want), linkDir, true},
		{"/other/iris", linkDir, false},
		{"iris", linkDir, false},                          // relative → linkDir/iris ≠ want
		{"iris", filepath.Dir(want), true},                // relative under bin dir matches
	}
	for _, tc := range cases {
		if got := symlinkPointsTo(tc.dest, want, tc.linkDir); got != tc.ok {
			t.Errorf("symlinkPointsTo(%q, %q, %q) = %v, want %v", tc.dest, want, tc.linkDir, got, tc.ok)
		}
	}
}

func TestRemoveInstallerSymlinkNoLink(t *testing.T) {
	// No PATH shim pointing at this fake target: must be a quiet no-op.
	if err := removeInstallerSymlink(filepath.Join(t.TempDir(), "iris")); err != nil {
		t.Fatalf("expected nil when link absent or unrelated, got %v", err)
	}
}

func TestRemoveInstallerSymlinkUserLocal(t *testing.T) {
	home := t.TempDir()
	// Point UserHomeDir at temp home so installerPATHLinks uses our dir.
	t.Setenv("HOME", home)
	// UserHomeDir on Linux reads $HOME.
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".iris", "bin", "iris")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(localBin, "iris")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation needs privilege on Windows: %v", err)
		}
		t.Fatal(err)
	}
	if err := removeInstallerSymlink(target); err != nil {
		t.Fatalf("remove user-local shim: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("shim still present: %v", err)
	}
}
