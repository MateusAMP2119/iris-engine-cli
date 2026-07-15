// Package pgtest provides a recording fake of the data database client seam
// (internal/pg). The Recorder captures every statement issued through the pg
// interface, in order, so a test can drive the real DDL/grant reconcile code and
// then diff the captured CREATE / ALTER / GRANT and trigger DDL byte-for-byte
// against golden files -- with no live Postgres. A golden diff is a contract diff.
//
// This is test-support infrastructure imported only by _test.go files. It is the
// seam by which E03/E04 prove their generated DDL: build a Recorder, issue the
// schema's statements through it, then pass Dump to internal/golden.Assert.
package pgtest

import (
	"context"
	"strings"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
)

// Recorder is a pg.DB that records the statements issued through it instead of
// executing them. The zero value is not usable; construct one with New.
type Recorder struct {
	mu    sync.Mutex
	stmts []string
	err   error
}

// New returns an empty recording data-database fake.
func New() *Recorder {
	return &Recorder{}
}

// compile-time proof the fake satisfies the seam it stands in for.
var _ pg.DB = (*Recorder)(nil)

// Exec records sql and returns the currently injected error (nil by default).
// The statement is recorded even when an error is injected, so a test can assert
// what was attempted before a modeled failure.
func (r *Recorder) Exec(_ context.Context, sql string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, sql)
	return r.err
}

// FailWith makes subsequent Exec calls return err, modeling a failing data
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
