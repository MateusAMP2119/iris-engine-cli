package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the daemon's leader-side build plane: the composition root that
// turns POST /pipeline/build into the dispatch build op over the single meta writer,
// the registry's run-target read, the content-addressed object store, and the exec
// seam (specification sections 1, 8, and 9). The toolchain subprocess is a fake --
// the exec seam's point -- while the hashing, object storage, and artifacts record
// are the real production path.

// fakeBuildTargets is a canned run-target read: pipeline name -> (folder, argv).
type fakeBuildTargets map[string]store.PipelineRunTarget

func (f fakeBuildTargets) PipelineRunTarget(_ context.Context, name string) (store.PipelineRunTarget, bool, error) {
	t, ok := f[name]
	return t, ok, nil
}

// buildToolFake is a fake exec.Runner toolchain: it records invocations and
// materializes output at the binary path the argv names, exiting 0.
type buildToolFake struct {
	output []byte
	specs  []exec.Spec
}

func (r *buildToolFake) Start(_ context.Context, spec exec.Spec) (exec.Handle, error) {
	r.specs = append(r.specs, spec)
	out := toolOutputPath(spec.Argv)
	if out == "" {
		return nil, os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(out, r.output, 0o755); err != nil { //nolint:gosec // an executable binary
		return nil, err
	}
	return builtHandle{}, nil
}

// toolOutputPath resolves the binary path a toolchain invocation writes.
func toolOutputPath(argv []string) string {
	var dist, name string
	for i := 0; i+1 < len(argv); i++ {
		switch argv[i] {
		case "-o", "--output":
			return argv[i+1]
		case "--distpath":
			dist = argv[i+1]
		case "--name":
			name = argv[i+1]
		}
	}
	if dist != "" && name != "" {
		return filepath.Join(dist, name)
	}
	return ""
}

// builtHandle is an already-succeeded fake toolchain process.
type builtHandle struct{}

func (builtHandle) PGID() int                      { return 4242 }
func (builtHandle) Wait() (exec.ExitStatus, error) { return exec.ExitStatus{Code: 0}, nil }
func (builtHandle) Kill() error                    { return nil }

// TestBuildPlaneRecordsHashAndBytes proves the daemon-composed build path end to
// end below the wire: an installed build orchestrator serves POST /pipeline/build
// by driving the pinned recipe toolchain in the pipeline's workspace folder,
// storing the produced binary's bytes in the object store under its content hash,
// and recording that hash in artifacts through the single meta writer -- and an
// uninstalled (or cleared) plane faults instead of building off-path.
//
// spec: S09/build-records-hash-and-bytes
func TestBuildPlaneRecordsHashAndBytes(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "etl"), 0o750); err != nil {
		t.Fatalf("mk pipeline folder: %v", err)
	}
	objects := store.NewObjectStore(filepath.Join(workspace, ".iris", "objects"))
	runner := &buildToolFake{output: []byte("#!ELF daemon-composed built binary")}
	targets := fakeBuildTargets{"etl": {Folder: "etl", Argv: []string{"python", "main.py"}}}

	plane := newBuildPlane(nil)

	// Not leading: no orchestrator installed, the mutation faults rather than
	// building off the single-writer path.
	if _, err := plane.BuildPipeline(context.Background(), api.PipelineBuildRequest{Pipeline: "etl"}); !errors.Is(err, api.ErrControlUnavailable) {
		t.Fatalf("uninstalled plane error = %v, want api.ErrControlUnavailable", err)
	}

	plane.install(newBuildOrchestrator(workspace, d, targets, objects, runner, nil))
	res, err := plane.BuildPipeline(context.Background(), api.PipelineBuildRequest{Pipeline: "etl"})
	if err != nil {
		t.Fatalf("BuildPipeline: %v", err)
	}

	// The hash names exactly the produced bytes, and the toolchain ran in the
	// pipeline's workspace folder.
	sum := sha256.Sum256(runner.output)
	if want := hex.EncodeToString(sum[:]); res.Hash != want {
		t.Errorf("result hash = %q, want %q", res.Hash, want)
	}
	if len(runner.specs) != 1 {
		t.Fatalf("toolchain invocations = %d, want 1", len(runner.specs))
	}
	if want := filepath.Join(workspace, "etl"); runner.specs[0].Dir != want {
		t.Errorf("toolchain ran in %q, want the pipeline folder %q", runner.specs[0].Dir, want)
	}

	// Bytes in the object store under the hash; the hash row through the writer.
	got, err := os.ReadFile(objects.Path(res.Hash))
	if err != nil || !bytes.Equal(got, runner.output) {
		t.Errorf("object bytes = %q (err %v), want %q", got, err, runner.output)
	}
	var inserted bool
	for _, s := range rec.Statements() {
		if strings.Contains(s.SQL, "INSERT INTO artifacts") && len(s.Args) > 0 && s.Args[0] == res.Hash {
			inserted = true
		}
	}
	if !inserted {
		t.Errorf("no artifacts insert recorded for hash %q\nstatements: %v", res.Hash, rec.Statements())
	}

	// An unregistered pipeline is an operation failure, not a silent success.
	if _, err := plane.BuildPipeline(context.Background(), api.PipelineBuildRequest{Pipeline: "ghost"}); err == nil {
		t.Error("building an unregistered pipeline returned nil error")
	}

	// Demotion clears the orchestrator: the mutation faults again.
	plane.clear()
	if _, err := plane.BuildPipeline(context.Background(), api.PipelineBuildRequest{Pipeline: "etl"}); !errors.Is(err, api.ErrControlUnavailable) {
		t.Errorf("cleared plane error = %v, want api.ErrControlUnavailable", err)
	}
}
