package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file holds the two source-level structural checks: the single-writer
// construction rule and the no-busy-retry rule. Both parse the module's own Go
// source (never executing the toolchain), like the import-graph checks, but they
// inspect identifiers and call sites rather than import edges.

// KindSingleWriter is a package other than internal/dispatch constructing a meta
// writer (calling store.NewWriter): a second meta write path.
const KindSingleWriter = "single-writer-construction"

// KindBusyRetry is an identifier in a read/write path named for a retry or backoff
// mechanism: a busy-retry the engine bans.
const KindBusyRetry = "busy-retry"

// storeNewWriter is the constructor whose call site the single-writer rule
// restricts to internal/dispatch.
const storeNewWriter = "NewWriter"

// dispatchPackage is the one package permitted to construct the meta writer.
const dispatchPackage = "dispatch"

// busyRetryPackages are the packages the no-busy-retry rule scans: the meta read
// and write paths (store, pg) and the dispatcher (dispatch). A busy-retry or
// backoff loop anywhere in these is forbidden; the rule is a documented name-based
// heuristic (no identifier may be named for a retry/backoff mechanism), which
// catches the loop constructs a busy-retry would introduce without false-flagging
// the doc comments that describe the absence of retry.
var busyRetryPackages = map[string]bool{"store": true, "pg": true, "dispatch": true}

// busyRetryTokens are the identifier substrings that name a retry/backoff
// mechanism. The match is case-insensitive and on identifiers only (not comments or
// string literals), so prose like "no busy-retry" never trips it.
var busyRetryTokens = []string{"retry", "backoff", "reattempt"}

// CheckSingleWriterConstruction returns a violation for every package -- other than
// internal/dispatch -- that constructs the meta writer by calling store.NewWriter.
// The dispatcher owns the sole meta writer; restricting the constructor's call
// site to dispatch means no other component can mint a second writer and open a
// second meta write path. module is the module path, used to recognize the store
// import.
func CheckSingleWriterConstruction(root, module string) ([]Violation, error) {
	storeImport := module + "/internal/store"
	var vs []Violation
	err := walkModuleGoFiles(root, func(rel, _ string, file *ast.File) {
		if rel == dispatchPackage {
			return // the one permitted construction site.
		}
		if callsStoreNewWriter(file, storeImport) {
			vs = append(vs, Violation{
				Kind:    KindSingleWriter,
				Subject: rel,
				Detail:  "calls store." + storeNewWriter + "; only internal/dispatch may construct the meta writer (the single-writer path)",
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return vs, nil
}

// CheckNoBusyRetry returns a violation for every identifier in a scanned package
// (store, pg, dispatch) named for a retry or backoff mechanism. Readers use plain
// MVCC connections with no busy-retry anywhere; the meta write path and the
// dispatcher likewise never spin on a failed operation. The
// check is a documented name-based heuristic over identifiers, so a busy-retry loop
// (which would name a retry counter, a backoff duration, or a reattempt helper) is
// caught while the doc comments describing the absence of retry are not.
func CheckNoBusyRetry(root string) ([]Violation, error) {
	var vs []Violation
	err := walkModuleGoFiles(root, func(rel, path string, file *ast.File) {
		if !busyRetryPackages[rel] {
			return
		}
		if name, ok := retryIdent(file); ok {
			vs = append(vs, Violation{
				Kind:    KindBusyRetry,
				Subject: rel,
				Detail: fmt.Sprintf("identifier %q in %s names a retry/backoff mechanism; readers and the meta write path carry no busy-retry",
					name, filepath.Base(path)),
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return vs, nil
}

// callsStoreNewWriter reports whether file calls store.NewWriter, resolving the
// store package's local import name (its alias, or the default "store").
func callsStoreNewWriter(file *ast.File, storeImport string) bool {
	local, imported := importLocalName(file, storeImport, "store")
	if !imported {
		return false
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != storeNewWriter {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
			found = true
			return false
		}
		return true
	})
	return found
}

// retryIdent returns the first identifier in file whose name (case-insensitive)
// contains a busy-retry token, and whether one was found.
func retryIdent(file *ast.File) (string, bool) {
	var hit string
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		lower := strings.ToLower(id.Name)
		for _, tok := range busyRetryTokens {
			if strings.Contains(lower, tok) {
				hit = id.Name
				return false
			}
		}
		return true
	})
	return hit, hit != ""
}

// importLocalName returns the local name a file binds to importPath (an explicit
// alias, or defaultName when imported without one), and whether the file imports it
// at all.
func importLocalName(file *ast.File, importPath, defaultName string) (string, bool) {
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, true
		}
		return defaultName, true
	}
	return "", false
}

// walkModuleGoFiles parses every non-test Go file under root's internal/ and cmd/
// trees (testdata, vendor, and hidden directories skipped) and calls fn with the
// file's repo-relative package key, its path, and its parsed AST. It mirrors
// LoadGraph's traversal so the two families of check agree on what a package is.
func walkModuleGoFiles(root string, fn func(rel, path string, file *ast.File)) error {
	fset := token.NewFileSet()
	collect := func(base string) error {
		return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if path != base && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path) //nolint:gosec // G304: path is a repo file under the module root the structural gate walks, never user or network input.
			if err != nil {
				return fmt.Errorf("arch: read %s: %w", path, err)
			}
			file, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return fmt.Errorf("arch: parse %s: %w", path, err)
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			fn(strings.TrimPrefix(filepath.ToSlash(rel), "internal/"), path, file)
			return nil
		})
	}
	if err := collect(filepath.Join(root, "internal")); err != nil {
		return fmt.Errorf("arch: walk internal: %w", err)
	}
	if err := collect(filepath.Join(root, "cmd")); err != nil {
		return fmt.Errorf("arch: walk cmd: %w", err)
	}
	return nil
}
