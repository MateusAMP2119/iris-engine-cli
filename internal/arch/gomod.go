package arch

import "strings"

// This file holds the hand-rolled go.mod parser. It exists because faithful
// go.mod parsing lives in golang.org/x/mod, which is not on the dependency
// allowlist this very package enforces; the format is small enough (a module
// line, a go line, and require directives in single-line or block form) to read
// directly with the standard library.

// Require is one require directive from go.mod: the module path, its version,
// and whether the require is marked // indirect (a transitive dependency the
// go tool records, not a direct import).
type Require struct {
	// Path is the required module path.
	Path string
	// Version is the required version.
	Version string
	// Indirect is true when the require carries a // indirect marker.
	Indirect bool
}

// GoMod is a parsed go.mod: its module path, its go directive version, and its
// require directives.
type GoMod struct {
	// Module is the module path from the module directive.
	Module string
	// GoVersion is the version from the go directive.
	GoVersion string
	// Requires are all require directives, direct and indirect, in file order.
	Requires []Require
}

// ParseGoMod parses go.mod source. It is pure: it reads only the bytes it is
// given, extracting the module path, the go version, and every require directive
// (single-line and block forms), and ignoring every other directive (replace,
// exclude, toolchain, retract) and all comments.
func ParseGoMod(src []byte) (*GoMod, error) {
	g := &GoMod{}
	inRequireBlock := false
	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if inRequireBlock {
			if line == ")" {
				inRequireBlock = false
				continue
			}
			if strings.HasPrefix(line, "//") {
				continue
			}
			if req, ok := parseRequireEntry(line); ok {
				g.Requires = append(g.Requires, req)
			}
			continue
		}
		if strings.HasPrefix(line, "//") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "require ("):
			inRequireBlock = true
		case strings.HasPrefix(line, "require "):
			if req, ok := parseRequireEntry(strings.TrimPrefix(line, "require ")); ok {
				g.Requires = append(g.Requires, req)
			}
		case strings.HasPrefix(line, "module "):
			g.Module = strings.TrimSpace(stripLineComment(strings.TrimPrefix(line, "module ")))
		case strings.HasPrefix(line, "go "):
			g.GoVersion = strings.TrimSpace(stripLineComment(strings.TrimPrefix(line, "go ")))
		default:
			// replace, exclude, toolchain, retract: not part of the require set the
			// allowlist and ban act on, ignored.
		}
	}
	return g, nil
}

// parseRequireEntry parses one require entry ("path version [// indirect]") from
// either form. It reports ok=false for a malformed line missing a path or
// version.
func parseRequireEntry(s string) (Require, bool) {
	indirect := false
	if i := strings.Index(s, "//"); i >= 0 {
		// The go tool's marker is exactly `// indirect`; match its leading token so
		// an ordinary trailing comment that merely mentions the word does not count.
		if fields := strings.Fields(s[i+2:]); len(fields) > 0 && fields[0] == "indirect" {
			indirect = true
		}
		s = s[:i]
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return Require{}, false
	}
	return Require{Path: fields[0], Version: fields[1], Indirect: indirect}, true
}

// stripLineComment removes a trailing // comment from a directive value.
func stripLineComment(s string) string {
	if i := strings.Index(s, "//"); i >= 0 {
		return s[:i]
	}
	return s
}

// DirectRequires returns the requires not marked // indirect: the module's
// direct dependencies, the set the allowlist bounds.
func (g *GoMod) DirectRequires() []Require {
	var out []Require
	for _, r := range g.Requires {
		if !r.Indirect {
			out = append(out, r)
		}
	}
	return out
}

// AllRequirePaths returns the module path of every require, direct and indirect:
// the module-local view of the dependency graph the forbidden-anywhere ban
// sweeps.
func (g *GoMod) AllRequirePaths() []string {
	out := make([]string, 0, len(g.Requires))
	for _, r := range g.Requires {
		out = append(out, r.Path)
	}
	return out
}
