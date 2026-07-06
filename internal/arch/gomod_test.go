package arch_test

import (
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/arch"
)

// TestParseGoMod proves the hand-rolled go.mod parser the allowlist check is
// built on: it extracts the module path, the go version, and every require
// directive in both single-line and block form, carries the // indirect marker
// through, and ignores comments and unrelated directives. The allowlist and
// forbidden-anywhere bans (specification section 9) can only be as trustworthy as
// this extraction, so it is proven directly on synthetic go.mod snippets and then
// run over the repo's real go.mod.
//
// spec: S09/dependency-allowlist
func TestParseGoMod(t *testing.T) {
	const src = `module example.com/m

go 1.26

require github.com/goccy/go-yaml v1.19.2

require (
	github.com/jackc/pgx/v5 v5.6.0
	github.com/spf13/cobra v1.8.1 // a trailing comment, not indirect
	golang.org/x/sys v0.20.0 // indirect
)

// a full-line comment is ignored
require golang.org/x/text v0.16.0 // indirect

replace example.com/old => example.com/new v1.2.3
exclude github.com/bad/mod v0.0.1
`
	g, err := arch.ParseGoMod([]byte(src))
	if err != nil {
		t.Fatalf("ParseGoMod: %v", err)
	}
	if g.Module != "example.com/m" {
		t.Errorf("Module = %q, want example.com/m", g.Module)
	}
	if g.GoVersion != "1.26" {
		t.Errorf("GoVersion = %q, want 1.26", g.GoVersion)
	}

	want := []arch.Require{
		{Path: "github.com/goccy/go-yaml", Version: "v1.19.2", Indirect: false},
		{Path: "github.com/jackc/pgx/v5", Version: "v5.6.0", Indirect: false},
		{Path: "github.com/spf13/cobra", Version: "v1.8.1", Indirect: false},
		{Path: "golang.org/x/sys", Version: "v0.20.0", Indirect: true},
		{Path: "golang.org/x/text", Version: "v0.16.0", Indirect: true},
	}
	if len(g.Requires) != len(want) {
		t.Fatalf("parsed %d requires, want %d: %+v", len(g.Requires), len(want), g.Requires)
	}
	for i, w := range want {
		if g.Requires[i] != w {
			t.Errorf("require[%d] = %+v, want %+v", i, g.Requires[i], w)
		}
	}

	// DirectRequires drops the two indirect entries; AllRequirePaths keeps every
	// path (direct and indirect), the surface the forbidden ban sweeps.
	if direct := g.DirectRequires(); len(direct) != 3 {
		t.Errorf("DirectRequires = %d entries, want 3: %+v", len(direct), direct)
	}
	if all := g.AllRequirePaths(); len(all) != 5 {
		t.Errorf("AllRequirePaths = %d entries, want 5: %v", len(all), all)
	}

	// The real repo go.mod parses and exposes a module path and at least the one
	// direct dependency present today.
	realMod, err := arch.LoadGoMod(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("LoadGoMod(real): %v", err)
	}
	if realMod.Module == "" {
		t.Error("real go.mod parsed with an empty module path")
	}
	if len(realMod.DirectRequires()) == 0 {
		t.Error("real go.mod parsed with no direct requires; expected at least goccy/go-yaml")
	}
}
