package store

import (
	"context"
	"fmt"
)

// This file is the registry teardown write surface: the meta writes that retire one
// declared pipeline in full (specification section 12, destructive ops item 1). Like
// every registry write it rides the single meta writer, and the whole retirement is
// one atomic meta transaction: the pipeline's runs and their inputs, dead-letter
// entries, artifacts, dependency edges, lane rows, and role/grants/credentials are
// deleted together, with the pipelines row deleted last, so a validation-passing
// destroy either retires the whole unit or leaves meta exactly as it was -- never a
// half-torn pipeline with a dangling edge, ledger, or worklist row.
//
// What this does NOT do, by design:
//   - It writes no run_summaries archival row. The spec has destroy write each
//     remaining run's archival summary before deleting it, so pruned lineage never
//     dangles; that archival tier is E05/E07's, and this teardown is deliberately
//     the retirement half. The summaries seam is documented at the dispatch layer
//     (dispatch.Destroyer) rather than silently skipped.
//   - It deletes no object-store bytes. The artifacts ROWS (the meta index) go in
//     this transaction; the content-addressed FILES under objects_path are a
//     filesystem side effect the dispatch layer's ObjectDeleter seam owns, since
//     they cannot ride a Postgres transaction.

// The retirement DELETE statements, one atomic transaction. Order is
// foreign-key-correct (children before parents) and ends with the pipelines row, so
// the batch never trips a constraint mid-flight and the registry root is retired
// last:
//
//	run_inputs      -> runs (both endpoints of the consumption ledger)
//	dead_letters    -> runs and pipelines (worklist + failed_upstream)
//	runs            -> pipelines and artifacts (history root; references artifacts.hash)
//	artifacts       -> pipelines (content-addressed index; runs already gone)
//	dependencies    -> pipelines (both endpoints of every edge touching the target)
//	lanes            (name-keyed, no FK: the target's lane membership row)
//	grants          -> roles (field-level rows for the target's roles)
//	credentials     -> roles (login secrets for the target's roles)
//	roles           -> pipelines (the access-ledger owner rows)
//	pipelines        (the registry root, deleted last)
const (
	// retireRunInputsSQL clears the consumption-ledger rows on either endpoint of the
	// target's runs: rows the target's runs recorded, and rows a downstream recorded
	// naming the target's runs as upstream, so no consumption row dangles.
	retireRunInputsSQL = `DELETE FROM run_inputs
WHERE run_id IN (SELECT id FROM runs WHERE pipeline = $1)
   OR upstream_run_id IN (SELECT id FROM runs WHERE pipeline = $1)`

	// retireDeadLettersSQL clears the worklist entries on the target's runs plus any
	// entry naming the target as the failed upstream that propagated, so no worklist
	// row dangles once the pipeline is gone.
	retireDeadLettersSQL = `DELETE FROM dead_letters
WHERE run_id IN (SELECT id FROM runs WHERE pipeline = $1)
   OR failed_upstream = $1`

	// retireRunsSQL clears the target's run history. It runs after run_inputs and
	// dead_letters (its children) and before artifacts (runs.artifact_hash references
	// artifacts.hash).
	retireRunsSQL = `DELETE FROM runs WHERE pipeline = $1`

	// retireArtifactsSQL clears the target's content-addressed artifact index rows.
	// The bytes under objects_path are removed separately (the dispatch ObjectDeleter
	// seam); this drops only the meta rows.
	retireArtifactsSQL = `DELETE FROM artifacts WHERE pipeline = $1`

	// retireDependenciesSQL clears every depends_on edge touching the target, on
	// either endpoint, so no edge is left half-hanging.
	retireDependenciesSQL = `DELETE FROM dependencies WHERE from_pipeline = $1 OR to_pipeline = $1`

	// retireLanesSQL clears the target's lane-membership row (lanes.pipeline is a
	// name, never an FK).
	retireLanesSQL = `DELETE FROM lanes WHERE pipeline = $1`

	// retireGrantsSQL clears the field-level grant rows for the target's roles, before
	// the roles they reference.
	retireGrantsSQL = `DELETE FROM grants WHERE pg_role IN (SELECT pg_role FROM roles WHERE pipeline = $1)`

	// retireCredentialsSQL clears the login secrets for the target's roles, before the
	// roles they reference.
	retireCredentialsSQL = `DELETE FROM credentials WHERE pg_role IN (SELECT pg_role FROM roles WHERE pipeline = $1)`

	// retireRolesSQL clears the target's access-ledger owner rows, after their grants
	// and credentials and before the pipelines row they reference.
	retireRolesSQL = `DELETE FROM roles WHERE pipeline = $1`

	// retirePipelineSQL deletes the registry root last: with every child row already
	// gone, the pipelines row can be removed without tripping a foreign key, and until
	// this point journal stamps still resolve the binary's name.
	retirePipelineSQL = `DELETE FROM pipelines WHERE name = $1`
)

// RetirePipeline retires one declared pipeline in full as one atomic meta
// transaction (specification section 12): it deletes the pipeline's runs and their
// consumption-ledger inputs, its dead-letter worklist entries, its artifact index
// rows, every dependency edge touching it, its lane-membership row, and its
// role/grants/credentials, with the pipelines registry row deleted last. Every
// statement is scoped to name, and the whole batch commits together or not at all,
// so a destroy never leaves a half-torn pipeline: either the whole unit is retired
// or meta is unchanged. It is a leader-only meta write, riding the single Writer.
//
// It touches no schema DDL and no journal, so the engine and the schemas/ tree stay
// intact; run_summaries (the archival tier) and the object-store bytes are not this
// method's concern (see the file header).
func (w *Writer) RetirePipeline(ctx context.Context, name string) error {
	stmts := []Statement{
		{SQL: retireRunInputsSQL, Args: []any{name}},
		{SQL: retireDeadLettersSQL, Args: []any{name}},
		{SQL: retireRunsSQL, Args: []any{name}},
		{SQL: retireArtifactsSQL, Args: []any{name}},
		{SQL: retireDependenciesSQL, Args: []any{name}},
		{SQL: retireLanesSQL, Args: []any{name}},
		{SQL: retireGrantsSQL, Args: []any{name}},
		{SQL: retireCredentialsSQL, Args: []any{name}},
		{SQL: retireRolesSQL, Args: []any{name}},
		{SQL: retirePipelineSQL, Args: []any{name}},
	}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer retire pipeline %q: %w", name, err)
	}
	return nil
}
