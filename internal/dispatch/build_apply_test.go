package dispatch_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// TestApplyNeverBuilds proves declare apply never triggers a pipeline build ("Build
// never folds into apply"); building happens only via the explicit `iris pipeline
// build` path. The applier and the builder share one dispatcher (the single meta
// writer) and one exec seam: a full pipeline apply of a buildable (python)
// declaration invokes no toolchain, writes no artifacts row, stores no object bytes,
// and registers the pipeline in artifact state source -- then the same environment's
// explicit build, and only it, invokes the toolchain once and records the artifact.
func TestApplyNeverBuilds(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	reg := storetest.NewRegistryFake()
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)

	objectsRoot := filepath.Join(t.TempDir(), "objects")
	runner := &toolchainRunner{Output: []byte("#!ELF built only when asked")}
	applier := dispatch.NewApplier(reg, d)
	builder := dispatch.NewBuilder(d, store.NewObjectStore(objectsRoot), runner)

	// A buildable declaration: the python run vector has a pinned recipe, so if
	// apply ever built, this apply would.
	decl := &declare.Pipeline{Name: "etl", Run: []string{"python", "main.py"}}
	if err := applier.ApplyPipeline(context.Background(), "etl", decl); err != nil {
		t.Fatalf("ApplyPipeline: %v", err)
	}

	// Apply wrote no artifact row: the real proof it never built, since an artifacts
	// insert is the sole meta trace a build leaves. (The toolchain runner is wired
	// only into the builder, not the applier, so a runner.calls()==0 check here would
	// be structurally vacuous -- the applier has no exec seam to reach it. The
	// meaningful contrast is drawn below: the same runner, driven by the explicit
	// build path, invokes the toolchain exactly once.)
	stmts := rec.Statements()
	if inserts := artifactInserts(stmts); len(inserts) != 0 {
		t.Errorf("apply recorded %d artifacts inserts, want 0", len(inserts))
	}

	// The apply registered the pipeline -- in artifact state source, not built.
	var registered bool
	for _, s := range stmts {
		if !strings.Contains(s.SQL, "pipelines") {
			continue
		}
		for _, a := range s.Args {
			if a == string(store.ArtifactSource) {
				registered = true
			}
			if a == string(store.ArtifactBuilt) {
				t.Errorf("apply registered the pipeline as built: %v", s)
			}
		}
	}
	if !registered {
		t.Fatalf("apply did not register the pipeline as artifact=source\nstatements: %v", stmts)
	}

	// Building happens only via the explicit build path: the same seams, driven by
	// the explicit op, invoke the toolchain exactly once and record the artifact.
	row, err := builder.Build(context.Background(), dispatch.BuildTarget{
		Pipeline: "etl", Dir: t.TempDir(), Run: decl.Run,
	})
	if err != nil {
		t.Fatalf("explicit Build: %v", err)
	}
	if got := runner.calls(); got != 1 {
		t.Errorf("explicit build invoked the toolchain %d times, want 1", got)
	}
	if inserts := artifactInserts(rec.Statements()); len(inserts) != 1 {
		t.Errorf("explicit build recorded %d artifacts inserts, want 1", len(inserts))
	} else if inserts[0].Args[0] != row.Hash {
		t.Errorf("explicit build recorded hash %v, want %q", inserts[0].Args[0], row.Hash)
	}
}
