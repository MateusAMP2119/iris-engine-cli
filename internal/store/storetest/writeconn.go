// This file adds a recording fake of the meta write connection (store.MetaTxConn):
// the registry write path. Unlike the DDL-only Recorder, WriteRecorder captures
// parameterized statements (SQL plus bound args) and their transaction grouping, so
// a test can assert the exact write set an apply issues and prove it committed as
// one atomic transaction -- with no live Postgres.
package storetest

import (
	"context"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// RecordedStatement is one statement a WriteRecorder captured: its SQL text and the
// bound positional args. Args holds copies, so a caller cannot reach back through a
// returned statement to mutate recorded state.
type RecordedStatement struct {
	// SQL is the recorded statement text.
	SQL string
	// Args are the bound positional arguments.
	Args []any
}

// WriteRecorder is a store.MetaTxConn that records the statements issued through it
// instead of executing them: single Exec calls and atomic ExecTx batches alike. A
// committed ExecTx records all its statements as one transaction; an ExecTx with an
// injected failure records nothing (modeling an atomic rollback), so a test can
// prove an all-or-nothing write left meta untouched. The zero value is not usable;
// construct one with NewWriteRecorder.
type WriteRecorder struct {
	mu     sync.Mutex
	stmts  []RecordedStatement   // every committed statement, in issue order
	txns   [][]RecordedStatement // one entry per committed ExecTx batch
	txFail error                 // injected: ExecTx returns this and records nothing
}

// NewWriteRecorder returns an empty recording meta write connection.
func NewWriteRecorder() *WriteRecorder { return &WriteRecorder{} }

// compile-time proof the fake satisfies the write and atomic-transaction seams.
var (
	_ store.MetaWriteConn = (*WriteRecorder)(nil)
	_ store.MetaTxConn    = (*WriteRecorder)(nil)
)

// Exec records one non-transactional statement.
func (r *WriteRecorder) Exec(_ context.Context, sql string, args ...any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, RecordedStatement{SQL: sql, Args: cloneArgs(args)})
	return nil
}

// ExecTx records stmts as one atomic transaction, or -- when a failure is injected
// with FailTx -- records nothing and returns that failure, modeling a rollback. A
// committed batch appends its statements both to the flat record and as one
// transaction group.
func (r *WriteRecorder) ExecTx(_ context.Context, stmts []store.Statement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.txFail != nil {
		return r.txFail // atomic rollback: commit nothing.
	}
	batch := make([]RecordedStatement, 0, len(stmts))
	for _, s := range stmts {
		batch = append(batch, RecordedStatement{SQL: s.SQL, Args: cloneArgs(s.Args)})
	}
	r.stmts = append(r.stmts, batch...)
	r.txns = append(r.txns, batch)
	return nil
}

// FailTx makes subsequent ExecTx calls fail with err and record nothing, modeling a
// meta transaction that aborts and rolls back. Pass nil to clear.
func (r *WriteRecorder) FailTx(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.txFail = err
}

// Statements returns a copy of every committed statement, in issue order.
func (r *WriteRecorder) Statements() []RecordedStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneStatements(r.stmts)
}

// Transactions returns a copy of the committed ExecTx batches, one slice per
// transaction, in commit order. A test asserts an apply committed as one atomic
// transaction by checking there is exactly one batch.
func (r *WriteRecorder) Transactions() [][]RecordedStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]RecordedStatement, len(r.txns))
	for i, batch := range r.txns {
		out[i] = cloneStatements(batch)
	}
	return out
}

// cloneArgs returns a shallow copy of args so a recorded statement does not alias
// the caller's slice.
func cloneArgs(args []any) []any {
	if len(args) == 0 {
		return nil
	}
	return append([]any(nil), args...)
}

// cloneStatements returns a deep-enough copy of stmts: a fresh slice with each
// statement's args copied.
func cloneStatements(stmts []RecordedStatement) []RecordedStatement {
	out := make([]RecordedStatement, len(stmts))
	for i, s := range stmts {
		out[i] = RecordedStatement{SQL: s.SQL, Args: cloneArgs(s.Args)}
	}
	return out
}
