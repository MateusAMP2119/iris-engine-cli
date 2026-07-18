package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// This file is the registry write surface: the meta writes that persist the
// declared world -- the pipelines registry, its depends_on graph, and the
// composed lanes. Every change rides the single meta writer, and each apply is
// one atomic meta transaction: a pipeline apply upserts the pipelines row and
// rewrites its dependency edges together; a composer apply rewrites a whole
// lane's rows together. All-or-nothing, so a validation-passing apply that fails
// mid-write leaves meta as it was, never half-registered.

// Artifact is a pipeline's artifact mode (pipelines.artifact): source (dev, the
// script run through its language runtime) or built (a content-addressed binary).
// Build and promote own this column; apply never overwrites an existing value.
type Artifact string

// The artifact modes (the pipelines.artifact CHECK value set).
const (
	// ArtifactSource is a dev pipeline: its script runs through its runtime.
	ArtifactSource Artifact = "source"
	// ArtifactBuilt is a built pipeline: a content-addressed self-contained binary.
	ArtifactBuilt Artifact = "built"
)

// DataMode is a pipeline's data mode (pipelines.data_mode): disposable
// (wipe-eligible) or permanent (promotion-locked, only ever once built). Promote
// owns this column; apply never overwrites an existing value.
type DataMode string

// The data modes (the pipelines.data_mode CHECK value set).
const (
	// DataDisposable is wipe-eligible pipeline data (the default at registration).
	DataDisposable DataMode = "disposable"
	// DataPermanent is promotion-locked pipeline data (requires built).
	DataPermanent DataMode = "permanent"
)

// PipelineRow is the pipelines-table registry row for one pipeline: exactly the
// columns of the pipelines table and nothing else. The declaration's env and
// env_file are deliberately absent: they resolve at run time and are never stored
// in meta, so a secret value has no column to land in.
type PipelineRow struct {
	// Name is the pipeline name, the pipelines primary key.
	Name string
	// Folder is the pipeline's folder (pipelines.folder).
	Folder string
	// Run is the declared argv vector, persisted as the run JSON column.
	Run []string
	// Artifact is the artifact mode written when the row is first inserted; a
	// re-apply preserves the existing value (build owns it).
	Artifact Artifact
	// DataMode is the data mode written when the row is first inserted; a re-apply
	// preserves the existing value (promote owns it).
	DataMode DataMode
	// LogSplit is the declared stdout/stderr split of the pipeline's run-log
	// recording contract (pipeline_logs.split); false when the block is omitted.
	LogSplit bool
	// LogStamp is the declared metadata stamp of the pipeline's run-log
	// recording contract (pipeline_logs.stamp); false when the block is omitted.
	LogStamp bool
	// Plugins are the declared plugin bindings (pipeline_plugins rows), rewritten wholesale on apply.
	Plugins []PipelinePluginRow
}

// PipelinePluginRow is one declared plugin binding (a pipeline_plugins row).
type PipelinePluginRow struct {
	// Alias is the binding's alias, the call-verb prefix.
	Alias string
	// Ref is the bound "name@version" reference.
	Ref string
	// Lifetime is the declared lifetime (run, lane, resident).
	Lifetime string
}

// DependencyEdge is one persisted depends_on edge: From (the dependent) depends on
// To (the upstream) -- "from depends_on to".
type DependencyEdge struct {
	// From is the dependent pipeline (dependencies.from_pipeline).
	From string
	// To is the upstream pipeline it depends on (dependencies.to_pipeline).
	To string
}

// Statement is one parameterized write statement: its SQL text and bound args. A
// slice of Statements is the unit an atomic registry apply submits, executed as one
// transaction so the whole slice commits or none of it does.
type Statement struct {
	// SQL is the statement text with positional ($1, $2, ...) placeholders.
	SQL string
	// Args are the bound positional arguments, one per placeholder.
	Args []any
}

// MetaTxConn is a MetaWriteConn that can additionally run a sequence of statements
// as one atomic transaction (all commit together, or none do). The leader's real
// meta connection wraps the sequence in a Postgres transaction; the recording fake
// records it atomically. The registry write methods require it, so a multi-statement
// apply is never split across un-atomic Execs.
type MetaTxConn interface {
	MetaWriteConn
	// ExecTx runs stmts as one atomic transaction. On any statement error the whole
	// batch is rolled back and the error returned; on success all statements commit.
	ExecTx(ctx context.Context, stmts []Statement) error
}

// RegistryReader reads the persisted registry (the pipelines, dependencies, and
// lanes tables) so the apply op can rebuild the dependency graph it validates a new
// declaration against, and the composer-destroy interlock can count a lane's members
// from meta rather than the workspace disk. It is a plain-MVCC read seam, never
// serialized through the single writer; a pgx-pool-backed implementation and a fake
// both satisfy it.
type RegistryReader interface {
	// RegisteredPipelines returns the names of every registered pipeline.
	RegisteredPipelines(ctx context.Context) ([]string, error)
	// DependencyEdges returns every persisted depends_on edge (from = dependent).
	DependencyEdges(ctx context.Context) ([]DependencyEdge, error)
	// LaneMembers returns the member pipeline names persisted in the lanes table for
	// the given lane, in walk (pos) order. The interlock counts these against the
	// registered set, so a member still registered but whose declaration file was
	// deleted from disk is never undercounted. An empty (or single-member) lane
	// returns no rows.
	LaneMembers(ctx context.Context, lane string) ([]string, error)
}

// errNoTxConn is returned when the write connection cannot run an atomic
// transaction: the registry apply must be all-or-nothing, so it fails loudly rather
// than splitting the change across un-atomic Execs. The public methods wrap it.
var errNoTxConn = errors.New("meta write connection does not support atomic transactions")

// The registry write statements. Each is a single parameterized statement; a
// method groups the ones an apply needs into one atomic transaction.
const (
	// pipelineUpsertSQL registers or refreshes a pipeline. On re-apply it updates
	// only the declared columns (folder, run); artifact and data_mode are preserved,
	// since build and promote -- not apply -- own a pipeline's lifecycle state.
	pipelineUpsertSQL = `INSERT INTO pipelines (name, folder, run, artifact, data_mode)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (name) DO UPDATE SET folder = EXCLUDED.folder, run = EXCLUDED.run`

	// pipelineLogsUpsertSQL records the pipeline's declared run-log recording
	// contract; a re-apply rewrites it wholesale (an omitted block re-applies the
	// engine default, false/false).
	pipelineLogsUpsertSQL = `INSERT INTO pipeline_logs (pipeline, split, stamp)
VALUES ($1, $2, $3)
ON CONFLICT (pipeline) DO UPDATE SET split = EXCLUDED.split, stamp = EXCLUDED.stamp`

	// deletePipelinePluginsSQL clears a pipeline's plugin bindings for the wholesale re-apply rewrite.
	deletePipelinePluginsSQL = `DELETE FROM pipeline_plugins WHERE pipeline = $1`

	// insertPipelinePluginSQL writes one declared plugin binding.
	insertPipelinePluginSQL = `INSERT INTO pipeline_plugins (pipeline, alias, ref, lifetime) VALUES ($1, $2, $3, $4)`

	// deleteDependenciesSQL clears a pipeline's outgoing depends_on edges so a
	// re-apply replaces them wholesale rather than accumulating stale rows.
	deleteDependenciesSQL = `DELETE FROM dependencies WHERE from_pipeline = $1`

	// insertDependencySQL writes one depends_on edge, from = the dependent.
	insertDependencySQL = `INSERT INTO dependencies (from_pipeline, to_pipeline) VALUES ($1, $2)`

	// deleteLaneSQL clears a lane's rows, the first half of the atomic full-lane
	// rewrite.
	deleteLaneSQL = `DELETE FROM lanes WHERE lane = $1`

	// insertLaneSQL writes one name-keyed lane row at its walk position.
	insertLaneSQL = `INSERT INTO lanes (lane, pipeline, pos) VALUES ($1, $2, $3)`

	// selectPipelineNamesSQL reads the registered pipeline names.
	selectPipelineNamesSQL = `SELECT name FROM pipelines ORDER BY name`

	// selectDependencyEdgesSQL reads the depends_on edges.
	selectDependencyEdgesSQL = `SELECT from_pipeline, to_pipeline FROM dependencies ORDER BY from_pipeline, to_pipeline`

	// selectLaneMembersSQL reads a lane's member names in walk (pos) order.
	selectLaneMembersSQL = `SELECT pipeline FROM lanes WHERE lane = $1 ORDER BY pos`
)

// RegisterPipeline persists a pipeline declaration as one atomic meta
// transaction: it upserts the pipelines row and replaces the pipeline's
// depends_on edges (a clearing delete plus one insert per current upstream). Both
// changes commit together or not at all. It never writes the lanes table -- a
// pipeline's lane position comes only from the composer's own apply. It is a
// leader-only meta write, riding the single Writer.
func (w *Writer) RegisterPipeline(ctx context.Context, row PipelineRow, dependsOn []string) error {
	runJSON, err := json.Marshal(row.Run)
	if err != nil {
		return fmt.Errorf("store: writer register pipeline %q: marshal run argv: %w", row.Name, err)
	}
	stmts := []Statement{
		{SQL: pipelineUpsertSQL, Args: []any{row.Name, row.Folder, string(runJSON), string(row.Artifact), string(row.DataMode)}},
		{SQL: pipelineLogsUpsertSQL, Args: []any{row.Name, row.LogSplit, row.LogStamp}},
		{SQL: deletePipelinePluginsSQL, Args: []any{row.Name}},
	}
	for _, p := range row.Plugins {
		stmts = append(stmts, Statement{SQL: insertPipelinePluginSQL, Args: []any{row.Name, p.Alias, p.Ref, p.Lifetime}})
	}
	stmts = append(stmts, Statement{SQL: deleteDependenciesSQL, Args: []any{row.Name}})
	for _, dep := range dedupeKeepOrder(dependsOn) {
		stmts = append(stmts, Statement{SQL: insertDependencySQL, Args: []any{row.Name, dep}})
	}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer register pipeline %q: %w", row.Name, err)
	}
	return nil
}

// RewriteLane rewrites a lane's membership as one atomic full-lane rewrite: it
// clears the lane's existing rows and re-inserts the given order, one name-keyed
// row per member at its walk position, in a single transaction. The members need
// not be registered -- lanes holds names, not foreign keys, and the runner skips
// unregistered ones. A lane needs two or more members to persist rows; an order
// of fewer than two clears the lane and writes no member row, since a
// single-member lane is nominal. It is a leader-only meta write, riding the
// single Writer.
func (w *Writer) RewriteLane(ctx context.Context, lane string, order []string) error {
	stmts := []Statement{{SQL: deleteLaneSQL, Args: []any{lane}}}
	if len(order) >= 2 {
		for i, member := range order {
			stmts = append(stmts, Statement{SQL: insertLaneSQL, Args: []any{lane, member, int64(i)}})
		}
	}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer rewrite lane %q: %w", lane, err)
	}
	return nil
}

// execTx runs stmts as one atomic transaction through the write connection's
// transactional capability. The leader's real meta connection and the recording
// fake both provide it; a connection without it fails loudly (errNoTxConn) rather
// than splitting an all-or-nothing apply across un-atomic Execs. It returns the raw
// error; the public methods own the "store:" prefix.
func (w *Writer) execTx(ctx context.Context, stmts []Statement) error {
	tx, ok := w.conn.(MetaTxConn)
	if !ok {
		return errNoTxConn
	}
	return tx.ExecTx(ctx, stmts)
}

// dedupeKeepOrder returns names with duplicates removed, preserving first-occurrence
// order. Duplicate depends_on entries would collide on the dependencies primary key
// and fail the whole transaction; deduping keeps the apply idempotent.
func dedupeKeepOrder(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// pgxRegistryReader is the pgx-pool-backed RegistryReader: plain MVCC reads over the
// reader pool, no session pinning and no busy-retry, so a candidate building the
// dependency graph never contends with the leader's single-writer path.
type pgxRegistryReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the registry read seam.
var _ RegistryReader = (*pgxRegistryReader)(nil)

// RegisteredPipelines reads the registered pipeline names in one plain MVCC query.
func (r *pgxRegistryReader) RegisteredPipelines(ctx context.Context) ([]string, error) {
	rows, err := r.pool.query(ctx, selectPipelineNamesSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read registered pipelines: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan pipeline name: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read registered pipelines: %w", err)
	}
	return out, nil
}

// DependencyEdges reads the depends_on edges in one plain MVCC query.
func (r *pgxRegistryReader) DependencyEdges(ctx context.Context) ([]DependencyEdge, error) {
	rows, err := r.pool.query(ctx, selectDependencyEdgesSQL)
	if err != nil {
		return nil, fmt.Errorf("store: read dependency edges: %w", err)
	}
	defer rows.Close()

	var out []DependencyEdge
	for rows.Next() {
		var e DependencyEdge
		if err := rows.Scan(&e.From, &e.To); err != nil {
			return nil, fmt.Errorf("store: scan dependency edge: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read dependency edges: %w", err)
	}
	return out, nil
}

// LaneMembers reads a lane's member names from the lanes table in walk (pos) order,
// in one plain MVCC query.
func (r *pgxRegistryReader) LaneMembers(ctx context.Context, lane string) ([]string, error) {
	rows, err := r.pool.query(ctx, selectLaneMembersSQL, lane)
	if err != nil {
		return nil, fmt.Errorf("store: read lane members: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan lane member: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read lane members: %w", err)
	}
	return out, nil
}
