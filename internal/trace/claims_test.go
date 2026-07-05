package trace_test

import (
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/trace"
)

// flatten collapses every claim in a parsed test file into id -> kind, plus a
// per-func view. Distinct fixtures use distinct ids per kind so nothing collides.
func flatten(tf *trace.TestFile) map[string]trace.ClaimKind {
	out := make(map[string]trace.ClaimKind)
	for _, fn := range tf.TestFuncs {
		for _, c := range fn.Claims {
			out[c.ID] = c.Kind
		}
	}
	return out
}

// TestExtractClaims proves the gate recognizes a contract claim expressed either
// as a Go subtest path (t.Run("Sxx/slug", ...)) or as a // spec: annotation on
// the test, on the doc comment or in the body, one test claiming several and one
// contract claimed by several, while ignoring subtest names that are not ids and
// strings that merely look like ids inside other literals.
//
// spec: S16/claims-via-subtest-or-annotation
func TestExtractClaims(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want map[string]trace.ClaimKind
	}{
		{
			name: "doc comment annotation",
			src: `package sample

import "testing"

// TestA proves a thing.
// spec: S16/manifest-row-schema
func TestA(t *testing.T) {}
`,
			want: map[string]trace.ClaimKind{
				"S16/manifest-row-schema": trace.KindAnnotation,
			},
		},
		{
			name: "body annotation",
			src: `package sample

import "testing"

func TestB(t *testing.T) {
	// spec: S16/exempt-needs-no-test
	_ = 1
}
`,
			want: map[string]trace.ClaimKind{
				"S16/exempt-needs-no-test": trace.KindAnnotation,
			},
		},
		{
			name: "subtest path",
			src: `package sample

import "testing"

func TestC(t *testing.T) {
	t.Run("S16/gate-fails-unclaimed-contract", func(t *testing.T) {})
	t.Run("plain human name", func(t *testing.T) {})
}
`,
			want: map[string]trace.ClaimKind{
				"S16/gate-fails-unclaimed-contract": trace.KindSubtest,
			},
		},
		{
			name: "both syntaxes and several ids",
			src: `package sample

import "testing"

// spec: S16/manifest-row-schema
// spec: S16/exempt-needs-no-test
func TestD(t *testing.T) {
	t.Run("S06.2/gate-awaits-latest-success", func(t *testing.T) {})
}
`,
			want: map[string]trace.ClaimKind{
				"S16/manifest-row-schema":          trace.KindAnnotation,
				"S16/exempt-needs-no-test":         trace.KindAnnotation,
				"S06.2/gate-awaits-latest-success": trace.KindSubtest,
			},
		},
		{
			name: "id-shaped string that is not a subtest name is not a claim",
			src: `package sample

import "testing"

func TestE(t *testing.T) {
	got := decode("S05/wipe-scope-rule")
	_ = got
}
`,
			want: map[string]trace.ClaimKind{},
		},
		{
			name: "spec word mid-sentence is not an annotation",
			src: `package sample

import "testing"

// TestF checks the spec: it must be robust.
func TestF(t *testing.T) {}
`,
			want: map[string]trace.ClaimKind{},
		},
		{
			name: "non-test funcs and helpers do not claim",
			src: `package sample

import "testing"

// spec: S16/manifest-row-schema
func helper(t *testing.T) {}

func TestifyLower(t *testing.T) {
	t.Run("S16/exempt-needs-no-test", func(t *testing.T) {})
}
`,
			// helper is not a Test func; TestifyLower has a lowercase rune after
			// Test so go test would not run it either -- neither claims.
			want: map[string]trace.ClaimKind{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tf, err := trace.ParseTestFile("sample_test.go", []byte(tt.src))
			if err != nil {
				t.Fatalf("ParseTestFile: %v", err)
			}
			got := flatten(tf)
			if len(got) != len(tt.want) {
				t.Fatalf("claims = %v, want %v", got, tt.want)
			}
			for id, kind := range tt.want {
				gk, ok := got[id]
				if !ok {
					t.Errorf("missing claim %s", id)
					continue
				}
				if gk != kind {
					t.Errorf("claim %s: kind = %v, want %v", id, gk, kind)
				}
			}
		})
	}
}

// TestClaimedIDsUnion proves several test files fold into one claimed-id set,
// the input the manifest->tests gap direction consumes.
//
// spec: S16/claims-via-subtest-or-annotation
func TestClaimedIDsUnion(t *testing.T) {
	a, err := trace.ParseTestFile("a_test.go", []byte(`package s
import "testing"
// spec: S01/apply-never-builds
func TestA(t *testing.T) {}
`))
	if err != nil {
		t.Fatalf("ParseTestFile a: %v", err)
	}
	b, err := trace.ParseTestFile("b_test.go", []byte(`package s
import "testing"
func TestB(t *testing.T) {
	t.Run("S01/apply-never-builds", func(t *testing.T) {})
	t.Run("S02/admin-dsn-precedence", func(t *testing.T) {})
}
`))
	if err != nil {
		t.Fatalf("ParseTestFile b: %v", err)
	}
	claimed := trace.ClaimedIDs([]*trace.TestFile{a, b})
	for _, id := range []string{"S01/apply-never-builds", "S02/admin-dsn-precedence"} {
		if !claimed[id] {
			t.Errorf("claimed[%s] = false, want true", id)
		}
	}
	if len(claimed) != 2 {
		t.Errorf("claimed set size = %d, want 2", len(claimed))
	}
}
