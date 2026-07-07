package dispatch_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/exec"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the dispatch-level build op (specification sections 1, 4, and 9):
// `iris pipeline build` drives the pipeline's one pinned recipe toolchain through the
// exec seam to compile the source into one self-contained binary, hashes the produced
// bytes, stores them in the content-addressed object store under that hash, and
// records the hash as an immutable artifacts row through the single meta writer.
// The toolchain subprocess is a fake (the exec seam's whole point: it records the
// invocation and materializes the binary bytes with no real PyInstaller/pkg -- real
// toolchain invocations are conformance work, E13); the hashing, object storage, and
// single-writer record are the real production path.

// toolchainRunner is a fake exec.Runner standing in for a build toolchain: it
// records every Start invocation and materializes Output at the binary path the
// invocation's argv names (the -o/--output value, or --distpath/--name joined),
// exiting with Exit. It is this file's stand-in for go build / PyInstaller / pkg.
type toolchainRunner struct {
	mu sync.Mutex
	// Output is the "compiled binary" the fake toolchain writes.
	Output []byte
	// Exit is the toolchain's exit code (non-zero models a failed compile).
	Exit int
	// SkipOutput models a broken toolchain that exits 0 without producing the binary.
	SkipOutput bool
	// Specs are the recorded invocations, in start order.
	Specs []exec.Spec
}

var _ exec.Runner = (*toolchainRunner)(nil)

func (r *toolchainRunner) Start(_ context.Context, spec exec.Spec) (exec.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Specs = append(r.Specs, spec)
	if r.Exit == 0 && !r.SkipOutput {
		out := outputPath(spec.Argv)
		if out == "" {
			return nil, os.ErrInvalid
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(out, r.Output, 0o755); err != nil { //nolint:gosec // an executable binary
			return nil, err
		}
	}
	return doneHandle{code: r.Exit}, nil
}

// calls returns how many toolchain invocations the fake saw.
func (r *toolchainRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Specs)
}

// outputPath resolves the binary path a toolchain invocation writes: the value
// after -o (go build) or --output (pkg), or --distpath/--name joined (PyInstaller
// one-file).
func outputPath(argv []string) string {
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

// doneHandle is an already-finished fake subprocess.
type doneHandle struct{ code int }

func (h doneHandle) PGID() int { return 4242 }
func (h doneHandle) Wait() (exec.ExitStatus, error) {
	st := exec.ExitStatus{Code: h.code}
	if h.code < 0 {
		st = exec.ExitStatus{Code: -1, Signaled: true, Signal: syscall.SIGKILL}
	}
	return st, nil
}
func (h doneHandle) Kill() error { return nil }

// buildHarness wires a Builder over a real Dispatcher (the single-writer path), a
// recording write connection, a real content-addressed object store under a temp
// root, and the fake toolchain runner.
type buildHarness struct {
	builder *dispatch.Builder
	rec     *storetest.WriteRecorder
	runner  *toolchainRunner
	objects *store.ObjectStore
}

func newBuildHarness(t *testing.T) buildHarness {
	t.Helper()
	rec := storetest.NewWriteRecorder()
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	objects := store.NewObjectStore(filepath.Join(t.TempDir(), "objects"))
	runner := &toolchainRunner{Output: []byte("#!ELF fake self-contained binary v1")}
	return buildHarness{
		builder: dispatch.NewBuilder(d, objects, runner),
		rec:     rec,
		runner:  runner,
		objects: objects,
	}
}

// pyTarget is a registered python pipeline's build target: the recipe is inferred
// from its declared run vector, nothing else.
func pyTarget(t *testing.T) dispatch.BuildTarget {
	t.Helper()
	return dispatch.BuildTarget{
		Pipeline: "etl",
		Dir:      t.TempDir(),
		Run:      []string{"python", "main.py"},
	}
}

// artifactInserts returns the recorded artifacts INSERT statements, in issue order.
func artifactInserts(stmts []storetest.RecordedStatement) []storetest.RecordedStatement {
	var out []storetest.RecordedStatement
	for _, s := range stmts {
		if strings.Contains(s.SQL, "INSERT INTO artifacts") {
			out = append(out, s)
		}
	}
	return out
}

// TestBuildSingleBinaryContentHash proves `iris pipeline build` compiles the source
// into ONE self-contained binary and records its content hash (specification
// section 1): exactly one toolchain invocation of the runtime's one pinned recipe
// (a python run vector selects PyInstaller one-file -- the engine's choice, never
// declared), and the recorded hash is the SHA-256 of exactly the produced binary's
// bytes, so the executed bytes are always identifiable from the hash alone.
//
// spec: S01/build-single-binary-content-hash
func TestBuildSingleBinaryContentHash(t *testing.T) {
	h := newBuildHarness(t)

	row, err := h.builder.Build(context.Background(), pyTarget(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// One binary: exactly one toolchain invocation, and it is the pinned recipe's
	// toolchain (PyInstaller in one-file mode for a python run vector).
	if got := h.runner.calls(); got != 1 {
		t.Fatalf("toolchain invocations = %d, want exactly 1 (one self-contained binary)", got)
	}
	argv := h.runner.Specs[0].Argv
	if argv[0] != "pyinstaller" {
		t.Errorf("toolchain argv[0] = %q, want the pinned python recipe %q", argv[0], "pyinstaller")
	}
	var onefile bool
	for _, a := range argv {
		if a == "--onefile" {
			onefile = true
		}
	}
	if !onefile {
		t.Errorf("toolchain argv %v lacks --onefile; the pinned python recipe is PyInstaller one-file", argv)
	}

	// The recorded hash identifies the executed bytes: SHA-256 of the binary.
	sum := sha256.Sum256(h.runner.Output)
	if want := hex.EncodeToString(sum[:]); row.Hash != want {
		t.Errorf("artifact hash = %q, want the binary's content hash %q", row.Hash, want)
	}
	if row.Pipeline != "etl" {
		t.Errorf("artifact pipeline = %q, want %q", row.Pipeline, "etl")
	}
	if row.SizeBytes != int64(len(h.runner.Output)) {
		t.Errorf("artifact size_bytes = %d, want %d", row.SizeBytes, len(h.runner.Output))
	}
}

// TestBuildRecordsHashAndBytes proves a successful build records the produced
// binary's content hash in the artifacts table (through the single meta writer)
// AND stores the binary's bytes in the object store under that hash -- and that a
// failed build records neither (specification section 9: "Building records the
// binary's content hash in artifacts and its bytes in the object store").
//
// spec: S09/build-records-hash-and-bytes
func TestBuildRecordsHashAndBytes(t *testing.T) {
	t.Run("success records hash row and object bytes", func(t *testing.T) {
		h := newBuildHarness(t)
		row, err := h.builder.Build(context.Background(), pyTarget(t))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		// The hash rides an artifacts insert on the single-writer path.
		inserts := artifactInserts(h.rec.Statements())
		if len(inserts) != 1 {
			t.Fatalf("artifacts inserts = %d, want exactly 1\nstatements: %v", len(inserts), h.rec.Statements())
		}
		args := inserts[0].Args
		if len(args) < 3 {
			t.Fatalf("artifacts insert args = %v, want (hash, pipeline, size_bytes)", args)
		}
		if args[0] != row.Hash {
			t.Errorf("artifacts insert hash arg = %v, want %q", args[0], row.Hash)
		}
		if args[1] != "etl" {
			t.Errorf("artifacts insert pipeline arg = %v, want %q", args[1], "etl")
		}
		if args[2] != row.SizeBytes {
			t.Errorf("artifacts insert size_bytes arg = %v, want %d", args[2], row.SizeBytes)
		}

		// The bytes live in the object store under the recorded hash.
		got, err := os.ReadFile(h.objects.Path(row.Hash))
		if err != nil {
			t.Fatalf("read object-store bytes under the recorded hash: %v", err)
		}
		if !bytes.Equal(got, h.runner.Output) {
			t.Errorf("object-store bytes = %q, want the built binary %q", got, h.runner.Output)
		}
	})

	t.Run("failed compile records nothing", func(t *testing.T) {
		h := newBuildHarness(t)
		h.runner.Exit = 1
		if _, err := h.builder.Build(context.Background(), pyTarget(t)); err == nil {
			t.Fatal("Build with a failing toolchain returned nil error")
		}
		if inserts := artifactInserts(h.rec.Statements()); len(inserts) != 0 {
			t.Errorf("failed build recorded %d artifacts inserts, want 0", len(inserts))
		}
	})

	t.Run("toolchain that produces no binary records nothing", func(t *testing.T) {
		h := newBuildHarness(t)
		h.runner.SkipOutput = true
		if _, err := h.builder.Build(context.Background(), pyTarget(t)); err == nil {
			t.Fatal("Build with a missing binary returned nil error")
		}
		if inserts := artifactInserts(h.rec.Statements()); len(inserts) != 0 {
			t.Errorf("binary-less build recorded %d artifacts inserts, want 0", len(inserts))
		}
	})
}

// TestArtifactRebuildInsertsNewRow proves artifact rows are immutable
// (specification section 4): a rebuild of changed source inserts a NEW row under a
// NEW hash -- the write path issues only plain INSERTs against artifacts, never an
// UPDATE, DELETE, or upsert -- the first object's bytes stay untouched in the
// object store, and the pipeline's current artifact is its newest row (the rebuild's).
//
// spec: S04/artifact-rebuild-inserts-new
func TestArtifactRebuildInsertsNewRow(t *testing.T) {
	h := newBuildHarness(t)
	target := pyTarget(t)

	first, err := h.builder.Build(context.Background(), target)
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}

	// The source changed; the rebuild produces different bytes.
	v1 := h.runner.Output
	h.runner.Output = []byte("#!ELF fake self-contained binary v2 (source changed)")
	second, err := h.builder.Build(context.Background(), target)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// A new row under a new hash.
	if second.Hash == first.Hash {
		t.Fatalf("rebuild reused hash %q; changed bytes must land under a new hash", first.Hash)
	}
	inserts := artifactInserts(h.rec.Statements())
	if len(inserts) != 2 {
		t.Fatalf("artifacts inserts after rebuild = %d, want 2 (one immutable row per build)", len(inserts))
	}
	if inserts[0].Args[0] != first.Hash || inserts[1].Args[0] != second.Hash {
		t.Errorf("artifacts insert hashes = [%v %v], want [%q %q]",
			inserts[0].Args[0], inserts[1].Args[0], first.Hash, second.Hash)
	}

	// Immutable rows: the write path never mutates artifacts -- inserts only.
	for _, s := range h.rec.Statements() {
		if !strings.Contains(s.SQL, "artifacts") {
			continue
		}
		if strings.Contains(s.SQL, "UPDATE") || strings.Contains(s.SQL, "DELETE") ||
			strings.Contains(s.SQL, "ON CONFLICT") {
			t.Errorf("artifacts write is not a plain insert (rows are immutable): %s", s.SQL)
		}
	}

	// The first artifact's bytes survive the rebuild untouched.
	got1, err := os.ReadFile(h.objects.Path(first.Hash))
	if err != nil || !bytes.Equal(got1, v1) {
		t.Errorf("first object after rebuild = %q (err %v), want %q untouched", got1, err, v1)
	}

	// Current artifact = the pipeline's newest row: the rebuild's row is the last
	// one recorded, and it is the row the build op reports as current.
	last := inserts[len(inserts)-1]
	if last.Args[0] != second.Hash {
		t.Errorf("newest artifacts row hash = %v, want the rebuild's %q", last.Args[0], second.Hash)
	}
}
