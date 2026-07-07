package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is the execution machinery under every data-surface read
// (specification section 7): the shared read pool on the data database. Each read
// checks a connection out of the shared pool, runs SET ROLE <pat_role>, executes a
// single-statement read-only transaction, and RESET ROLE on release; the same
// mechanics serve /q and /data alike. Request handling never assembles SQL: only
// an engine-built ReadStatement -- a fixed parameterized SELECT -- can enter the
// pool, each pooled session prepares that fixed text on first use (session-scoped
// prepared statements), and every request value rides as a bound parameter. The
// pool can never address the meta database (BuildReadPoolConn refuses it), so
// engine storage stays unreachable through the data surface, and a read-only
// transaction plus the SELECT-only statement gate leave no mutation path anywhere
// on the read surface: the journal is readable, never writable, and all changes go
// via the control plane.

// The fixed session-control statements of the read cycle. They are constants --
// never assembled -- and they are the only non-prepared SQL the pool ever issues.
const (
	beginReadOnlySQL = "BEGIN READ ONLY"
	commitSQL        = "COMMIT"
	rollbackSQL      = "ROLLBACK"
	resetRoleSQL     = "RESET ROLE"
)

// ErrReadPoolMetaDatabase is returned by BuildReadPoolConn when the requested
// database is the meta database: the read pool serves the data surface (/data,
// /q) and never addresses engine storage (specification section 7: meta is a
// separate database the data surface cannot reach).
var ErrReadPoolMetaDatabase = errors.New("store: the read pool serves the data surface and never addresses the meta database")

// readStatementNameRe is the shape of a prepared-statement name: a bare lowercase
// identifier, assigned by the engine (q_<endpoint> or a data_<schema>_<table>_<hash>
// shape name), never caller input.
var readStatementNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ReadStatement is one engine-built read: a stable prepared-statement name and its
// fixed parameterized SELECT text. It is the only thing the read pool executes,
// and NewReadStatement is its only constructor -- a single SELECT or nothing -- so
// no write, DDL, or piggybacked second statement can ever enter the pool, and the
// journal stays readable but never writable through the API.
type ReadStatement struct {
	// name is the session-scoped prepared-statement name.
	name string
	// text is the fixed parameterized SELECT text ($n placeholders, no values).
	text string
}

// NewReadStatement builds a ReadStatement from an engine-assigned name and a fixed
// parameterized text. It refuses anything that is not exactly one SELECT: the text
// must begin with SELECT and must not contain a statement separator (a lone
// trailing semicolon is tolerated, as the endpoint compiler emits one). The name
// must be a bare lowercase identifier.
func NewReadStatement(name, text string) (ReadStatement, error) {
	if !readStatementNameRe.MatchString(name) {
		return ReadStatement{}, fmt.Errorf("store: read statement name %q is not a bare identifier", name)
	}
	body := strings.TrimSpace(text)
	body = strings.TrimSuffix(body, ";")
	if body == "" {
		return ReadStatement{}, fmt.Errorf("store: read statement %q has no text", name)
	}
	if first := strings.Fields(body)[0]; !strings.EqualFold(first, "SELECT") {
		return ReadStatement{}, fmt.Errorf("store: read statement %q is not a SELECT; the read surface has no mutation path", name)
	}
	if strings.ContainsRune(body, ';') {
		return ReadStatement{}, fmt.Errorf("store: read statement %q is not a single statement", name)
	}
	return ReadStatement{name: name, text: text}, nil
}

// Name returns the statement's prepared-statement name.
func (s ReadStatement) Name() string { return s.name }

// Text returns the statement's fixed parameterized SELECT text.
func (s ReadStatement) Text() string { return s.text }

// IsZero reports whether the statement is the zero value (no statement held).
func (s ReadStatement) IsZero() bool { return s.name == "" }

// ReadRows is the row cursor a read hands its consumer: pgx.Rows satisfies it.
// The pool closes the cursor when the consumer returns, so a consumer never
// retains it past the callback.
type ReadRows interface {
	// Next advances to the next row, reporting whether one exists.
	Next() bool
	// Scan copies the current row's columns into dest.
	Scan(dest ...any) error
	// Err returns the error that ended iteration, if any.
	Err() error
}

// readSession is one checked-out pooled connection: the minimal surface the read
// cycle runs against, so every mechanic is provable against a recording fake with
// no live Postgres. The pgx-backed session adapts a *pgxpool.Conn.
type readSession interface {
	// exec runs one fixed session-control statement (SET ROLE, BEGIN READ ONLY,
	// COMMIT, ROLLBACK, RESET ROLE).
	exec(ctx context.Context, sql string) error
	// prepared reports whether this session has already prepared name.
	prepared(name string) bool
	// prepare prepares the fixed statement text under name on this session.
	prepare(ctx context.Context, name, text string) error
	// queryPrepared executes the session's prepared statement name with bound args.
	queryPrepared(ctx context.Context, name string, args ...any) (poolRows, error)
	// release returns the session to the pool; broken destroys it instead (a
	// session whose role could not be reset must never be reused).
	release(broken bool)
}

// readAcquirer is the shared pool the read cycle checks sessions out of.
type readAcquirer interface {
	// acquire checks one session out of the shared pool.
	acquire(ctx context.Context) (readSession, error)
}

// ReadPool is the shared read pool on the data database (specification section
// 7): every /q and /data read runs through it, one checkout per read, with the
// SET ROLE / read-only transaction / RESET ROLE cycle wrapped around a
// session-scoped prepared statement.
type ReadPool struct {
	pool readAcquirer
}

// newReadPool builds a ReadPool over an acquirer seam (a fake in tests, the pgx
// adapter in production).
func newReadPool(pool readAcquirer) *ReadPool { return &ReadPool{pool: pool} }

// NewPgxReadPool wraps a *pgxpool.Pool -- built by the daemon from
// BuildReadPoolConn's data-database connection string -- as the shared read pool.
func NewPgxReadPool(pool *pgxpool.Pool) *ReadPool {
	return newReadPool(&pgxReadAcquirer{pool: pool})
}

// BuildReadPoolConn assembles the shared read pool's connection string: the
// engine's own login role and engine-minted credential on the data database. It
// refuses the meta database outright (ErrReadPoolMetaDatabase) -- the pool serves
// the data surface, and engine storage is a separate database that /data and /q
// can never address.
func BuildReadPoolConn(params ScopedConnParams, role string, secret Secret) (ScopedConn, error) {
	if params.Database == MetaDatabase {
		return ScopedConn{}, fmt.Errorf("store: build read pool connection: %w", ErrReadPoolMetaDatabase)
	}
	return BuildScopedConn(params, role, secret)
}

// Read executes one engine-built statement as role: it checks a session out of
// the shared pool, runs SET ROLE <role>, opens a read-only transaction, prepares
// the statement's fixed text if this session has not yet (session-scoped, first
// use), executes it with args bound positionally, hands the rows to consume,
// closes the transaction, and always issues RESET ROLE before the session goes
// back -- a session whose reset fails is destroyed, never reused. The transaction
// contains exactly one statement, and it is read-only, so no route through this
// pool can mutate anything.
func (p *ReadPool) Read(ctx context.Context, role string, stmt ReadStatement, args []any, consume func(ReadRows) error) (err error) {
	if role == "" {
		return fmt.Errorf("store: read pool: %w", ErrInvalidRoleOwner)
	}
	if stmt.IsZero() {
		return errors.New("store: read pool: empty read statement")
	}
	if consume == nil {
		return errors.New("store: read pool: nil row consumer")
	}

	sess, err := p.pool.acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: read pool: acquire: %w", err)
	}
	defer func() {
		// RESET ROLE on release, always -- success and failure alike. A session
		// that cannot shed its role is destroyed so the next checkout can never
		// run as the previous caller.
		if rerr := sess.exec(ctx, resetRoleSQL); rerr != nil {
			sess.release(true)
			if err == nil {
				err = fmt.Errorf("store: read pool: reset role: %w", rerr)
			}
			return
		}
		sess.release(false)
	}()

	if err := sess.exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return fmt.Errorf("store: read pool: set role %q: %w", role, err)
	}
	if err := sess.exec(ctx, beginReadOnlySQL); err != nil {
		return fmt.Errorf("store: read pool: begin read-only transaction: %w", err)
	}
	if err := p.readInTxn(ctx, sess, stmt, args, consume); err != nil {
		// The transaction is aborted or unusable; roll it back before the role
		// resets. A rollback failure marks the session broken via the deferred
		// reset (the reset will fail on a dead session too), so the read error
		// stays the one surfaced.
		_ = sess.exec(ctx, rollbackSQL)
		return err
	}
	if err := sess.exec(ctx, commitSQL); err != nil {
		return fmt.Errorf("store: read pool: commit read-only transaction: %w", err)
	}
	return nil
}

// readInTxn runs the single statement of the read-only transaction: prepare on
// first use, execute with bound params, consume, close.
func (p *ReadPool) readInTxn(ctx context.Context, sess readSession, stmt ReadStatement, args []any, consume func(ReadRows) error) error {
	if !sess.prepared(stmt.name) {
		if err := sess.prepare(ctx, stmt.name, stmt.text); err != nil {
			return fmt.Errorf("store: read pool: prepare %s: %w", stmt.name, err)
		}
	}
	rows, err := sess.queryPrepared(ctx, stmt.name, args...)
	if err != nil {
		return fmt.Errorf("store: read pool: execute %s: %w", stmt.name, err)
	}
	cerr := consume(rows)
	rerr := rows.Err()
	rows.Close()
	if cerr != nil {
		return fmt.Errorf("store: read pool: consume %s: %w", stmt.name, cerr)
	}
	if rerr != nil {
		return fmt.Errorf("store: read pool: read %s: %w", stmt.name, rerr)
	}
	return nil
}

// pgxReadAcquirer adapts a *pgxpool.Pool to the readAcquirer seam.
type pgxReadAcquirer struct {
	pool *pgxpool.Pool
}

// compile-time proof the pgx adapter satisfies the pool seam.
var _ readAcquirer = (*pgxReadAcquirer)(nil)

func (a *pgxReadAcquirer) acquire(ctx context.Context) (readSession, error) {
	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return &pgxReadSession{conn: conn}, nil
}

// pgxReadSession adapts one *pgxpool.Conn checkout to the readSession seam.
type pgxReadSession struct {
	conn *pgxpool.Conn
}

// compile-time proof the pgx session satisfies the session seam.
var _ readSession = (*pgxReadSession)(nil)

func (s *pgxReadSession) exec(ctx context.Context, sql string) error {
	_, err := s.conn.Exec(ctx, sql)
	return err
}

// prepared always reports false: pgx.Conn.Prepare is idempotent and keeps its own
// session-scoped statement cache, so delegating every first-use check to it sends
// Parse to the server exactly once per session and statement name -- the
// session-scoped prepared-statement behavior, without a shadow cache that could
// drift from the connection's real state.
func (s *pgxReadSession) prepared(string) bool { return false }

func (s *pgxReadSession) prepare(ctx context.Context, name, text string) error {
	_, err := s.conn.Conn().Prepare(ctx, name, text)
	return err
}

func (s *pgxReadSession) queryPrepared(ctx context.Context, name string, args ...any) (poolRows, error) {
	rows, err := s.conn.Query(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *pgxReadSession) release(broken bool) {
	if broken {
		// Destroy the underlying connection so the pool never hands out a session
		// still wearing a caller's role.
		_ = s.conn.Conn().Close(context.Background())
	}
	s.conn.Release()
}
