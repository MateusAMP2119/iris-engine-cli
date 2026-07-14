// This file adds a recording fake of the meta DDL seam (store.Execer). The
// Recorder captures every statement bootstrap and EnsureSchema issue, in order,
// so a test can drive the embedded meta DDL and diff the captured CREATE
// DATABASE / CREATE TABLE / CREATE INDEX statements byte-for-byte against golden
// files -- with no live Postgres. A golden
// diff is a contract diff.
package storetest

import (
	"context"
	"strings"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// Recorder is a store.Execer that records the statements issued through it
// instead of executing them. The zero value is not usable; construct one with
// NewRecorder.
type Recorder struct {
	mu    sync.Mutex
	stmts []string
	err   error
}

// NewRecorder returns an empty recording meta-DDL fake.
func NewRecorder() *Recorder { return &Recorder{} }

// compile-time proof the fake satisfies the seam it stands in for.
var _ store.Execer = (*Recorder)(nil)

// Exec records sql and returns the currently injected error (nil by default).
// The statement is recorded even when an error is injected, so a test can assert
// what was attempted before a modeled failure.
func (r *Recorder) Exec(_ context.Context, sql string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, sql)
	return r.err
}

// FailWith makes subsequent Exec calls return err, modeling a failing meta
// database. Pass nil to clear.
func (r *Recorder) FailWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Statements returns a copy of the recorded statements, in issue order.
func (r *Recorder) Statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stmts...)
}

// Dump renders the recording as golden-file bytes: each recorded statement,
// verbatim and in order, separated by a blank line and terminated by a trailing
// newline. It is the canonical form a test passes to internal/golden.Assert.
func (r *Recorder) Dump() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stmts) == 0 {
		return nil
	}
	return []byte(strings.Join(r.stmts, "\n\n") + "\n")
}
