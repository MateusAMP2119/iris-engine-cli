package declare_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/fixtures"
)

// goldenDecl reads a golden-workspace iris-declare.yaml at the given path
// elements under the golden workspace root, failing the test on a read error.
func goldenDecl(t *testing.T, elem ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{fixtures.WorkspaceGolden()}, elem...)...)
	b, err := os.ReadFile(path) //nolint:gosec // G304: a checked-in fixture path under the trusted repo tree.
	if err != nil {
		t.Fatalf("read golden declaration %v: %v", elem, err)
	}
	return b
}

// invalidDecl reads the iris-declare.yaml of the named invalid-declaration
// fixture directory, failing the test on a read error.
func invalidDecl(t *testing.T, name string) []byte {
	t.Helper()
	//nolint:gosec // G304: a checked-in fixture path under the trusted repo tree.
	b, err := os.ReadFile(filepath.Join(fixtures.InvalidDeclaration(name), "iris-declare.yaml"))
	if err != nil {
		t.Fatalf("read invalid declaration %q: %v", name, err)
	}
	return b
}

// TestEightFieldWhitelist proves that pipeline-declaration parsing admits
// exactly the eight allowed fields and rejects every other key, naming it.
func TestEightFieldWhitelist(t *testing.T) {
	t.Run("S03/eight-field-whitelist", func(t *testing.T) {
		// Accept: the golden load_orders declaration exercises seven of the eight
		// fields (name, run, env, env_file, lane, reads, writes, depends_on).
		good := goldenDecl(t, "pipelines", "ingest", "load_orders", "iris-declare.yaml")
		decl, err := declare.ParseDeclaration(good)
		if err != nil {
			t.Fatalf("golden load_orders rejected: %v", err)
		}
		if decl.Kind != declare.KindPipeline || decl.Pipeline == nil {
			t.Fatalf("golden load_orders parsed as %v, want a pipeline", decl.Kind)
		}

		// Accept: a minimal declaration with only the two required fields.
		if _, err := declare.ParseDeclaration([]byte("name: p\nrun: [python, main.py]\n")); err != nil {
			t.Fatalf("minimal name+run declaration rejected: %v", err)
		}

		// Reject: the unknown_field fixture carries `schedule`, outside the eight.
		if _, err := declare.ParseDeclaration(invalidDecl(t, "unknown_field")); err == nil {
			t.Error("unknown_field fixture accepted; expected rejection")
		}

		// Reject: every forbidden key the spec calls out, each named in the error.
		forbidden := []string{
			"language", "build", "param", "retry", "schedule",
			"triggers", "executor", "deadline", "timeout", "state",
		}
		for _, key := range forbidden {
			src := []byte("name: p\nrun: [python, main.py]\n" + key + ": x\n")
			_, err := declare.ParseDeclaration(src)
			if err == nil {
				t.Errorf("forbidden field %q accepted; expected rejection", key)
				continue
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("rejecting %q, error %q does not name the offending key", key, err)
			}
		}

		// Reject: `order` is a composer field, never a pipeline field; a file that
		// carries both run and order is neither shape.
		if _, err := declare.ParseDeclaration([]byte("name: p\nrun: [python, main.py]\norder: [a, b]\n")); err == nil {
			t.Error("declaration with both run and order accepted; expected rejection")
		}

		// Composer-shape parse guards (the composer analogue of the pipeline
		// required-field rules): a composer carries a non-empty lane and a
		// non-empty order; an empty order or a missing/blank lane is malformed.
		composerReject := []struct {
			name string
			src  string
		}{
			{"empty-order", "lane: ingest\norder: []\n"},
			{"missing-lane", "order: [a, b]\n"},
			{"empty-lane", "lane: \"\"\norder: [a, b]\n"},
			{"blank-lane", "lane: \"   \"\norder: [a, b]\n"},
		}
		for _, tc := range composerReject {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := declare.ParseDeclaration([]byte(tc.src)); err == nil {
					t.Errorf("malformed composer (%s) accepted; expected rejection", tc.name)
				}
			})
		}
		// Accept: a well-formed composer (non-empty lane and order).
		comp, err := declare.ParseDeclaration([]byte("lane: ingest\norder: [a, b]\n"))
		if err != nil {
			t.Fatalf("well-formed composer rejected: %v", err)
		}
		if comp.Kind != declare.KindComposer || comp.Composer == nil {
			t.Fatalf("composer parsed as %v, want a composer", comp.Kind)
		}
	})
}

// TestNameRequired proves that a pipeline declaration missing the required
// string field name is rejected.
func TestNameRequired(t *testing.T) {
	t.Run("S03/name-required", func(t *testing.T) {
		// Reject: the missing_name fixture (run + lane, no name).
		if _, err := declare.ParseDeclaration(invalidDecl(t, "missing_name")); err == nil {
			t.Error("missing_name fixture accepted; expected rejection")
		}

		cases := []struct {
			name string
			src  string
		}{
			{"absent", "run: [python, main.py]\n"},
			{"empty-string", "name: \"\"\nrun: [python, main.py]\n"},
			{"blank-string", "name: \"   \"\nrun: [python, main.py]\n"},
			{"non-string", "name: [a, b]\nrun: [python, main.py]\n"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := declare.ParseDeclaration([]byte(tc.src)); err == nil {
					t.Errorf("declaration with %s name accepted; expected rejection", tc.name)
				}
			})
		}

		// Accept: a present, non-empty string name.
		decl, err := declare.ParseDeclaration([]byte("name: extract\nrun: [python, main.py]\n"))
		if err != nil {
			t.Fatalf("valid name rejected: %v", err)
		}
		if decl.Pipeline.Name != "extract" {
			t.Errorf("Name = %q, want extract", decl.Pipeline.Name)
		}
	})
}

// TestRunRequiredArgvList proves that run is required and must parse as a plain
// string list (an argv vector); a missing or non-list run is rejected.
func TestRunRequiredArgvList(t *testing.T) {
	t.Run("S03/run-required-argv-list", func(t *testing.T) {
		// Reject: the missing_run fixture (name only).
		if _, err := declare.ParseDeclaration(invalidDecl(t, "missing_run")); err == nil {
			t.Error("missing_run fixture accepted; expected rejection")
		}

		cases := []struct {
			name string
			src  string
		}{
			{"absent", "name: p\nlane: ingest\n"},
			{"scalar-string", "name: p\nrun: python main.py\n"},
			{"mapping", "name: p\nrun: {cmd: python}\n"},
			{"empty-list", "name: p\nrun: []\n"},
			{"non-string-element", "name: p\nrun: [python, 3]\n"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := declare.ParseDeclaration([]byte(tc.src)); err == nil {
					t.Errorf("run %s accepted; expected rejection", tc.name)
				}
			})
		}

		// Accept: a plain string list parses to the argv vector, in order.
		decl, err := declare.ParseDeclaration([]byte("name: p\nrun: [python, main.py, --flag]\n"))
		if err != nil {
			t.Fatalf("valid argv list rejected: %v", err)
		}
		if got, want := strings.Join(decl.Pipeline.Run, " "), "python main.py --flag"; got != want {
			t.Errorf("Run = %q, want %q", got, want)
		}
	})
}

// TestSampleDependencySplit pins the sample declarations' use of depends_on
// (data gate) separately from composer order (walk position source). load_orders
// declares depends_on, reset_counters declares none; the lane walk order is
// still the composer's, proving independence.
//
// spec: S13/sample-dependency-split
func TestSampleDependencySplit(t *testing.T) {
	t.Run("S13/sample-dependency-split", func(t *testing.T) {
		// load_orders declares depends_on: [extract_orders]
		loadSrc := goldenDecl(t, "pipelines", "ingest", "load_orders", "iris-declare.yaml")
		loadDecl, err := declare.ParseDeclaration(loadSrc)
		if err != nil {
			t.Fatalf("parse load_orders: %v", err)
		}
		if loadDecl.Kind != declare.KindPipeline || loadDecl.Pipeline == nil {
			t.Fatalf("load_orders not a pipeline decl")
		}
		if got := loadDecl.Pipeline.DependsOn; len(got) != 1 || got[0] != "extract_orders" {
			t.Errorf("load_orders depends_on = %v, want [extract_orders]", got)
		}

		// reset_counters declares no depends_on
		resetSrc := goldenDecl(t, "pipelines", "ingest", "reset_counters", "iris-declare.yaml")
		resetDecl, err := declare.ParseDeclaration(resetSrc)
		if err != nil {
			t.Fatalf("parse reset_counters: %v", err)
		}
		if resetDecl.Kind != declare.KindPipeline || resetDecl.Pipeline == nil {
			t.Fatalf("reset_counters not a pipeline decl")
		}
		if got := resetDecl.Pipeline.DependsOn; len(got) != 0 {
			t.Errorf("reset_counters depends_on = %v, want [] (none)", got)
		}

		// Composer order is independent of the depends_on edges.
		// (walk positions come from composer alone)
		compSrc := goldenDecl(t, "pipelines", "ingest", "iris-declare.yaml")
		compDecl, err := declare.ParseDeclaration(compSrc)
		if err != nil {
			t.Fatalf("parse composer: %v", err)
		}
		if compDecl.Kind != declare.KindComposer || compDecl.Composer == nil {
			t.Fatalf("ingest composer not parsed as composer")
		}
		wantOrder := []string{"extract_orders", "reset_counters", "load_orders"}
		if got := compDecl.Composer.Order; !equalOrder(got, wantOrder) {
			t.Errorf("composer order = %v, want %v (independent of depends_on)", got, wantOrder)
		}
	})
}

// equalOrder is a local helper (dupe of unexported in sibling test) for order compare.
func equalOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
