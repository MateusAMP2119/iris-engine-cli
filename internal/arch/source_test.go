package arch_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/arch"
)

// writeSource writes a Go source file at root/rel, creating parent directories.
func writeSource(t *testing.T, root, rel, src string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestSingleWriterConstruction proves the meta writer has exactly one construction
// site: only internal/dispatch may call store.NewWriter, so no other package can
// mint a second meta writer and open a second write path -- the dispatcher is the
// sole meta writer. A planted call outside dispatch is a violation; the
// dispatcher's own call is fine; the real repo constructs the writer only in
// dispatch.
func TestSingleWriterConstruction(t *testing.T) {
	t.Run("dispatcher-sole-meta-writer", func(t *testing.T) {
		const module = "example.com/m"
		storeImport := module + "/internal/store"

		t.Run("a NewWriter call outside dispatch is a violation", func(t *testing.T) {
			root := t.TempDir()
			writeSource(t, root, "internal/rogue/w.go", `package rogue
import "`+storeImport+`"
func open(c store.MetaWriteConn) *store.Writer { return store.NewWriter(c) }
`)
			vs, err := arch.CheckSingleWriterConstruction(root, module)
			if err != nil {
				t.Fatalf("CheckSingleWriterConstruction: %v", err)
			}
			if len(vs) != 1 || vs[0].Subject != "rogue" {
				t.Fatalf("planted rogue NewWriter = %v, want one violation for rogue", vs)
			}
		})

		t.Run("the dispatcher may construct the writer", func(t *testing.T) {
			root := t.TempDir()
			writeSource(t, root, "internal/dispatch/d.go", `package dispatch
import "`+storeImport+`"
func mk(c store.MetaWriteConn) *store.Writer { return store.NewWriter(c) }
`)
			vs, err := arch.CheckSingleWriterConstruction(root, module)
			if err != nil {
				t.Fatalf("CheckSingleWriterConstruction: %v", err)
			}
			if len(vs) != 0 {
				t.Errorf("dispatch's NewWriter call was flagged: %v", vs)
			}
		})

		t.Run("the real repo constructs the writer only in dispatch", func(t *testing.T) {
			root := filepath.Join("..", "..")
			gm, err := arch.LoadGoMod(filepath.Join(root, "go.mod"))
			if err != nil {
				t.Fatalf("LoadGoMod: %v", err)
			}
			vs, err := arch.CheckSingleWriterConstruction(root, gm.Module)
			if err != nil {
				t.Fatalf("CheckSingleWriterConstruction: %v", err)
			}
			if len(vs) != 0 {
				t.Errorf("a package other than dispatch constructs store.Writer: %v", vs)
			}
		})
	})
}

// TestNoBusyRetry proves the meta read/write and dispatch paths carry no busy-retry
// or backoff loop anywhere. The check is a documented name-based heuristic: no
// identifier in store, pg, or dispatch may be
// named for a retry or backoff mechanism. A planted retry loop is caught; the real
// repo has none.
func TestNoBusyRetry(t *testing.T) {
	t.Run("readers-plain-mvcc-no-retry", func(t *testing.T) {
		t.Run("a retry/backoff identifier in a scanned package is a violation", func(t *testing.T) {
			root := t.TempDir()
			writeSource(t, root, "internal/store/bad.go", `package store
import "time"
func readWithRetry() { for i := 0; i < 3; i++ { time.Sleep(time.Second) } }
`)
			vs, err := arch.CheckNoBusyRetry(root)
			if err != nil {
				t.Fatalf("CheckNoBusyRetry: %v", err)
			}
			if len(vs) == 0 {
				t.Fatalf("planted readWithRetry in store was not flagged")
			}
			if vs[0].Subject != "store" {
				t.Errorf("violation subject = %q, want store", vs[0].Subject)
			}
		})

		t.Run("a backoff loop in dispatch is a violation", func(t *testing.T) {
			root := t.TempDir()
			writeSource(t, root, "internal/dispatch/bad.go", `package dispatch
var backoff = 3
`)
			vs, err := arch.CheckNoBusyRetry(root)
			if err != nil {
				t.Fatalf("CheckNoBusyRetry: %v", err)
			}
			if len(vs) == 0 {
				t.Error("planted backoff in dispatch was not flagged")
			}
		})

		t.Run("the real store/pg/dispatch packages carry no busy-retry", func(t *testing.T) {
			root := filepath.Join("..", "..")
			vs, err := arch.CheckNoBusyRetry(root)
			if err != nil {
				t.Fatalf("CheckNoBusyRetry: %v", err)
			}
			if len(vs) != 0 {
				t.Errorf("a scanned package names a retry/backoff construct (busy-retry): %v", vs)
			}
		})
	})
}
