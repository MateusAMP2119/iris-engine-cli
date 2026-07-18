package store

import (
	"context"
	"fmt"
)

// This file is the turn-protocol run-record write surface (#206). Under the turn
// protocol a loop run's record is minted only when its turn produces something to
// record: a producing turn mints a RUNNING run just before its data transaction
// (so a crash between the two leaves a running row for reconciliation, never data
// without a record) and completes it with the turn's stamps after; a failed turn
// mints its run directly dead-lettered with the worklist row in the same atomic
// statement; a quiet turn mints nothing at all. Every method rides the single
// Writer like the rest of the run-record path.

// TurnRunRecord is the input to the turn-run mints: the run identity fields a
// turn carries when it records.
type TurnRunRecord struct {
	// Pipeline is the pipeline the turn ran (runs.pipeline).
	Pipeline string
	// Cause is why the turn ran (runs.cause): loop for a lane turn.
	Cause RunCause
	// DeclarationChecksum is the declaration hash the turn executed under.
	DeclarationChecksum string
	// ArtifactHash is the built binary's content hash, nil for a dev run.
	ArtifactHash *string
	// Handle is the resident process's group id (runs.handle).
	Handle int
	// LogRef is the per-run log reference (runs.log_ref), empty for none.
	LogRef string
	// ConsumedUpstreamRunIDs are the upstream runs the turn's gate resolved,
	// one run_inputs row each.
	ConsumedUpstreamRunIDs []int64
	// Plugins are the run's digest-pinned plugin resolutions (#215), one run_plugins row each.
	Plugins []RunPluginPin
	// Calls are the turn's serviced plugin calls (#215), one run_plugin_calls row each.
	Calls []RunPluginCall
}

// RunPluginPin is one run_plugins row: a plugin resolved and digest-verified at run start.
type RunPluginPin struct {
	// Alias is the declared binding alias.
	Alias string
	// Name is the resolved plugin name.
	Name string
	// Version is the resolved plugin version.
	Version string
	// Digest is the installed binary's verified sha256.
	Digest string
	// InstanceID names the service instance the run attached (#215 stage 3):
	// two runs naming one id shared its state; empty for a tool plugin.
	InstanceID string
}

// RunPluginCall is one run_plugin_calls row: a serviced call's provenance.
type RunPluginCall struct {
	// Seq is the call's 1-based order within the run.
	Seq int64
	// Alias is the declared binding alias the verb rode.
	Alias string
	// Verb is the manifest verb called.
	Verb string
	// ArgsDigest is the sha256 of the call's args bytes.
	ArgsDigest string
	// Outcome is "ok" or "err".
	Outcome string
	// ResponseDigest is the sha256 of the ok result bytes; empty for err.
	ResponseDigest string
	// Error is the failure message; empty for ok.
	Error string
}

// pluginLegsSQL are the shared plugin-ledger CTE legs (#215): the run's pins and
// its serviced calls, empty arrays inserting nothing. They ride both mints so a
// turn's external effects land atomically with its run row.
const pluginLegsSQL = `, pins AS (
    INSERT INTO run_plugins (run_id, alias, name, version, digest, instance_id)
    SELECT new_run.id, p.alias, p.name, p.version, p.digest, NULLIF(p.instance_id, '')
    FROM new_run, unnest(%[1]s::text[], %[2]s::text[], %[3]s::text[], %[4]s::text[], %[5]s::text[]) AS p(alias, name, version, digest, instance_id)
)
INSERT INTO run_plugin_calls (run_id, seq, alias, verb, args_digest, outcome, response_digest, error, recorded_at)
SELECT new_run.id, c.seq, c.alias, c.verb, c.args_digest, c.outcome, NULLIF(c.response_digest, ''), NULLIF(c.err, ''), now()::text
FROM new_run, unnest(%[6]s::bigint[], %[7]s::text[], %[8]s::text[], %[9]s::text[], %[10]s::text[], %[11]s::text[], %[12]s::text[]) AS c(seq, alias, verb, args_digest, outcome, response_digest, err)`

// createTurnRunSQL mints a producing turn's run directly in the running state,
// with its handle and log reference, its consumption ledger, and its plugin
// ledgers, in one atomic CTE (the createRunSQL shape with the state and handle
// fixed at mint).
var createTurnRunSQL = `WITH new_run AS (
    INSERT INTO runs
        (pipeline, state, cause, artifact_hash, declaration_checksum, handle, log_ref, recorded_at)
    VALUES ($1, $2, $3, $4, $5, NULLIF($6, 0), NULLIF($7, ''), now()::text)
    RETURNING id
), inputs AS (
    INSERT INTO run_inputs (run_id, upstream_run_id)
    SELECT new_run.id, upstream
    FROM new_run, unnest($8::bigint[]) AS upstream
)` + fmt.Sprintf(pluginLegsSQL, "$9", "$10", "$11", "$12", "$13", "$14", "$15", "$16", "$17", "$18", "$19", "$20")

// completeTurnRunSQL closes a producing turn's run: the guarded running ->
// succeeded transition stamping exit code zero, the turn's snapshot pin
// (LSN, journal floor and ceiling), and the log reference in one statement.
const completeTurnRunSQL = `UPDATE runs
SET state = $1, exit_code = 0, snapshot_lsn = $2, journal_floor = $3, journal_ceiling = $4, log_ref = NULLIF($5, '')
WHERE id = $6 AND state = $7`

// stampRunLogRefSQL records a run's log reference after the fact: the failed-turn
// mint cannot know its run-id-keyed log path before the id exists.
const stampRunLogRefSQL = `UPDATE runs SET log_ref = NULLIF($1, '') WHERE id = $2`

// deadLetterTurnRunSQL mints a failed turn's run directly dead-lettered with its
// worklist row, consumption ledger, and plugin ledgers, one atomic CTE (the
// DeadLetterPropagated shape with the cause and reason carried by the turn). A
// failed turn's calls still record: the external effects happened.
var deadLetterTurnRunSQL = `WITH new_run AS (
    INSERT INTO runs
        (pipeline, state, cause, artifact_hash, declaration_checksum, handle, log_ref, recorded_at)
    VALUES ($1, $2, $3, $4, $5, NULLIF($6, 0), NULLIF($7, ''), now()::text)
    RETURNING id
), letter AS (
    INSERT INTO dead_letters (run_id, reason, error)
    SELECT id, $8, $9 FROM new_run
), inputs AS (
    INSERT INTO run_inputs (run_id, upstream_run_id)
    SELECT new_run.id, upstream
    FROM new_run, unnest($10::bigint[]) AS upstream
)` + fmt.Sprintf(pluginLegsSQL, "$11", "$12", "$13", "$14", "$15", "$16", "$17", "$18", "$19", "$20", "$21", "$22")

// pluginLegArgs renders a record's plugin pins and calls as the twelve
// positional array args the pluginLegsSQL legs unnest, in leg order.
func pluginLegArgs(rec TurnRunRecord) []any {
	pinAlias := make([]string, len(rec.Plugins))
	pinName := make([]string, len(rec.Plugins))
	pinVersion := make([]string, len(rec.Plugins))
	pinDigest := make([]string, len(rec.Plugins))
	pinInstance := make([]string, len(rec.Plugins))
	for i, p := range rec.Plugins {
		pinAlias[i], pinName[i], pinVersion[i], pinDigest[i] = p.Alias, p.Name, p.Version, p.Digest
		pinInstance[i] = p.InstanceID
	}
	callSeq := make([]int64, len(rec.Calls))
	callAlias := make([]string, len(rec.Calls))
	callVerb := make([]string, len(rec.Calls))
	callArgs := make([]string, len(rec.Calls))
	callOutcome := make([]string, len(rec.Calls))
	callResponse := make([]string, len(rec.Calls))
	callErr := make([]string, len(rec.Calls))
	for i, c := range rec.Calls {
		callSeq[i], callAlias[i], callVerb[i] = c.Seq, c.Alias, c.Verb
		callArgs[i], callOutcome[i], callResponse[i], callErr[i] = c.ArgsDigest, c.Outcome, c.ResponseDigest, c.Error
	}
	return []any{pinAlias, pinName, pinVersion, pinDigest, pinInstance,
		callSeq, callAlias, callVerb, callArgs, callOutcome, callResponse, callErr}
}

// CreateTurnRun mints a producing turn's run row directly in the running state
// with its consumption ledger, one atomic meta transaction. The caller commits
// the turn's data transaction next and completes the run after, so a crash
// between the two leaves a running run for the next leader's reconciliation.
func (w *Writer) CreateTurnRun(ctx context.Context, rec TurnRunRecord) error {
	var artifactHash any
	if rec.ArtifactHash != nil {
		artifactHash = *rec.ArtifactHash
	}
	args := []any{
		rec.Pipeline,
		string(RunRunning),
		string(rec.Cause),
		artifactHash,
		rec.DeclarationChecksum,
		rec.Handle,
		rec.LogRef,
		rec.ConsumedUpstreamRunIDs,
	}
	stmts := []Statement{{SQL: createTurnRunSQL, Args: append(args, pluginLegArgs(rec)...)}}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer create turn run for pipeline %q: %w", rec.Pipeline, err)
	}
	return nil
}

// CompleteTurnRun records a producing turn's successful terminal transition and
// its snapshot pin in one guarded statement: running -> succeeded, exit code
// zero, the turn's LSN and journal window, and the run-id-keyed log reference,
// so the whole terminal state is a single meta write.
func (w *Writer) CompleteTurnRun(ctx context.Context, id string, snapshotLSN string, journalFloor, journalCeiling int64, logRef string) error {
	if err := w.conn.Exec(ctx, completeTurnRunSQL, RunSucceeded, snapshotLSN, journalFloor, journalCeiling, logRef, id, RunRunning); err != nil {
		return fmt.Errorf("store: writer complete turn run %s: %w", id, err)
	}
	return nil
}

// StampRunLogRef records a run's log reference after its row exists: a failed
// turn's run is minted before its run-id-keyed log can be opened, so the
// reference lands in this one follow-up write.
func (w *Writer) StampRunLogRef(ctx context.Context, id string, logRef string) error {
	if err := w.conn.Exec(ctx, stampRunLogRefSQL, logRef, id); err != nil {
		return fmt.Errorf("store: writer stamp run log ref %s: %w", id, err)
	}
	return nil
}

// The pre-minted-run plugin ledger inserts: a queued or immediate manual run's
// row exists before its turn drives, so pins and calls land in this follow-up
// atomic transaction instead of the mint CTE.
const (
	recordRunPluginsSQL = `INSERT INTO run_plugins (run_id, alias, name, version, digest, instance_id)
SELECT $1, p.alias, p.name, p.version, p.digest, NULLIF(p.instance_id, '')
FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[]) AS p(alias, name, version, digest, instance_id)`

	recordRunPluginCallsSQL = `INSERT INTO run_plugin_calls (run_id, seq, alias, verb, args_digest, outcome, response_digest, error, recorded_at)
SELECT $1, c.seq, c.alias, c.verb, c.args_digest, c.outcome, NULLIF(c.response_digest, ''), NULLIF(c.err, ''), now()::text
FROM unnest($2::bigint[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[]) AS c(seq, alias, verb, args_digest, outcome, response_digest, err)`
)

// RecordRunPlugins writes a pre-minted run's plugin pins and serviced calls in
// one atomic transaction (#215); empty inputs write nothing.
func (w *Writer) RecordRunPlugins(ctx context.Context, id string, rec TurnRunRecord) error {
	if len(rec.Plugins) == 0 && len(rec.Calls) == 0 {
		return nil
	}
	legs := pluginLegArgs(rec)
	stmts := []Statement{
		{SQL: recordRunPluginsSQL, Args: append([]any{id}, legs[:5]...)},
		{SQL: recordRunPluginCallsSQL, Args: append([]any{id}, legs[5:]...)},
	}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer record run plugins %s: %w", id, err)
	}
	return nil
}

// stampRunStartedSQL records a mint-first running run's process handle and log
// reference once its subprocess exists, guarded on the running state.
const stampRunStartedSQL = `UPDATE runs SET handle = $1, log_ref = NULLIF($2, '') WHERE id = $3 AND state = $4`

// StampRunStarted records the process-group handle and log reference of a run
// that was minted directly running (an immediate manual turn mints running so
// the queued-manual pickup can never race it into a double execution); the
// subprocess spawns after the mint, so the handle lands in this follow-up write.
func (w *Writer) StampRunStarted(ctx context.Context, id string, pgid int, logRef string) error {
	if err := w.conn.Exec(ctx, stampRunStartedSQL, pgid, logRef, id, RunRunning); err != nil {
		return fmt.Errorf("store: writer stamp run started %s: %w", id, err)
	}
	return nil
}

// DeadLetterTurnRun mints a failed turn's run directly dead-lettered with its
// worklist row and consumption ledger, one atomic meta transaction: a failed
// turn always records (the dead-letter worklist is the product), even though a
// quiet turn records nothing.
func (w *Writer) DeadLetterTurnRun(ctx context.Context, rec TurnRunRecord, reason DeadLetterReason, detail string) error {
	var artifactHash any
	if rec.ArtifactHash != nil {
		artifactHash = *rec.ArtifactHash
	}
	var errDetail any
	if detail != "" {
		errDetail = detail
	}
	args := []any{
		rec.Pipeline,
		string(RunDeadLettered),
		string(rec.Cause),
		artifactHash,
		rec.DeclarationChecksum,
		rec.Handle,
		rec.LogRef,
		string(reason),
		errDetail,
		rec.ConsumedUpstreamRunIDs,
	}
	stmts := []Statement{{SQL: deadLetterTurnRunSQL, Args: append(args, pluginLegArgs(rec)...)}}
	if err := w.execTx(ctx, stmts); err != nil {
		return fmt.Errorf("store: writer dead-letter turn run for pipeline %q: %w", rec.Pipeline, err)
	}
	return nil
}
