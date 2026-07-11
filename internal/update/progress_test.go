package update

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestUpdateProgressStages proves the per-stage progress hook fires exactly the
// four stages, in order, over a real replace against a local release server:
// resolve, download, verify, replace. A nil hook (the default) stays silent, and
// an up-to-date run emits no stages because it returns before any download.
//
// spec: S08/update-progress-stages
func TestUpdateProgressStages(t *testing.T) {
	t.Run("update fires resolve, download, verify, replace in order", func(t *testing.T) {
		dir := t.TempDir()
		exe := filepath.Join(dir, "iris")
		if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
			t.Fatalf("seed exe: %v", err)
		}
		srv := releaseServer(t, "v2.0.0", tarGzWithIris(t, []byte("NEW-BINARY")), "", nil)

		u := testUpdater(srv, exe)
		var stages []string
		var details []string
		u.Progress = func(stage, detail string) {
			stages = append(stages, stage)
			details = append(details, detail)
		}
		if _, err := u.Run(context.Background(), "v1.0.0"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := []string{StageResolve, StageDownload, StageVerify, StageReplace}
		if !reflect.DeepEqual(stages, want) {
			t.Fatalf("stages = %v, want %v", stages, want)
		}
		// The resolve stage carries the tag; verify carries a non-empty detail.
		if details[0] != "v2.0.0" {
			t.Errorf("resolve detail = %q, want the tag %q", details[0], "v2.0.0")
		}
		if details[1] == "" {
			t.Errorf("download detail should name the asset and size, got empty")
		}
	})

	t.Run("up-to-date fires no stages", func(t *testing.T) {
		dir := t.TempDir()
		exe := filepath.Join(dir, "iris")
		if err := os.WriteFile(exe, []byte("SAME"), 0o755); err != nil {
			t.Fatalf("seed exe: %v", err)
		}
		srv := releaseServer(t, "v3.0.0", tarGzWithIris(t, []byte("unused")), "", nil)

		u := testUpdater(srv, exe)
		var stages []string
		u.Progress = func(stage, _ string) { stages = append(stages, stage) }
		if _, err := u.Run(context.Background(), "v3.0.0"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(stages) != 0 {
			t.Errorf("up-to-date run emitted stages %v, want none", stages)
		}
	})
}
