package store

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
)

// This file is the meta-backed StatsSource: the production implementation of the
// stats read seam. It draws one plain-MVCC snapshot of the runs, dead-letter
// worklist, persisted composer, registry, and checkpoint chain from the reader
// pool, exactly like the other pgx readers -- no session pinning, no busy-retry
// -- and feeds it to the pure BuildStats composition. The storetest fake
// satisfies the same seam at integration tier; this is the one that runs against
// a live Postgres.
//
// The journal-side counters (capture counter, wipe-eligible slice, journal size,
// hot rows) live in the data journal, whose lifecycle machinery is E07's and whose
// rows live in the data database, not meta. Until those readers land this source
// reports them zero (the JournalStats seam's documented default), so the rollup is
// honest rather than fabricating a count it cannot read.

// SQL the stats source reads. Each is a plain SELECT: an MVCC snapshot, no locking
// clause, no advisory-lock interplay.
const (
	statsRunsSQL        = `SELECT id, pipeline, state, exit_code FROM runs ORDER BY id`
	statsDeadLettersSQL = `SELECT run_id, reason FROM dead_letters ORDER BY run_id`
	statsLaneMembersSQL = `SELECT lane, pipeline FROM lanes ORDER BY lane, pos`
	statsPipelinesSQL   = `SELECT name FROM pipelines ORDER BY name`
	statsCheckpointsSQL = `SELECT seq, digest, location FROM journal_checkpoints ORDER BY seq`
)

// pgxStatsSource is the pgx-pool-backed StatsSource: plain MVCC reads over the
// reader pool, so the stats rollup never contends with the leader's single-writer
// path.
type pgxStatsSource struct {
	pool readPool
}

// compile-time proof the meta-backed source satisfies the stats read seam.
var _ StatsSource = (*pgxStatsSource)(nil)

// newPgxStatsSource builds a stats source over a pooled-query seam.
func newPgxStatsSource(pool readPool) *pgxStatsSource { return &pgxStatsSource{pool: pool} }

// Runs reads the run snapshot in ordering-identity (id) order. The run id doubles
// as the ordering identity (Seq): the rollup picks last-values by Seq, never a
// clock. exit_code is nullable -- nil when the run carries none.
func (s *pgxStatsSource) Runs(ctx context.Context, filter RunFilter) ([]Run, error) {
	rows, err := s.pool.query(ctx, statsRunsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: stats read runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var id int64
		var pipeline, state string
		var exit *int
		if err := rows.Scan(&id, &pipeline, &state, &exit); err != nil {
			return nil, fmt.Errorf("store: stats scan run: %w", err)
		}
		run := Run{
			ID:       strconv.FormatInt(id, 10),
			Pipeline: pipeline,
			State:    RunState(state),
			ExitCode: exit,
			Seq:      id,
		}
		if runMatchesFilter(run, filter) {
			out = append(out, run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stats read runs: %w", err)
	}
	return out, nil
}

// DeadLetters reads the outstanding worklist entries: the run and its closed-set
// reason (the rollup counts depth and per-reason from these).
func (s *pgxStatsSource) DeadLetters(ctx context.Context) ([]DeadLetterEntry, error) {
	rows, err := s.pool.query(ctx, statsDeadLettersSQL)
	if err != nil {
		return nil, fmt.Errorf("store: stats read dead letters: %w", err)
	}
	defer rows.Close()

	var out []DeadLetterEntry
	for rows.Next() {
		var runID int64
		var reason string
		if err := rows.Scan(&runID, &reason); err != nil {
			return nil, fmt.Errorf("store: stats scan dead letter: %w", err)
		}
		out = append(out, DeadLetterEntry{RunID: strconv.FormatInt(runID, 10), Reason: DeadLetterReason(reason)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stats read dead letters: %w", err)
	}
	return out, nil
}

// LaneMembers reads the persisted composer rows: each pipeline's lane membership,
// in lane then walk (pos) order.
func (s *pgxStatsSource) LaneMembers(ctx context.Context) ([]LaneMember, error) {
	rows, err := s.pool.query(ctx, statsLaneMembersSQL)
	if err != nil {
		return nil, fmt.Errorf("store: stats read lane members: %w", err)
	}
	defer rows.Close()

	var out []LaneMember
	for rows.Next() {
		var m LaneMember
		if err := rows.Scan(&m.Lane, &m.Pipeline); err != nil {
			return nil, fmt.Errorf("store: stats scan lane member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stats read lane members: %w", err)
	}
	return out, nil
}

// PipelineNames reads every registered pipeline's name, in name order.
func (s *pgxStatsSource) PipelineNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.query(ctx, statsPipelinesSQL)
	if err != nil {
		return nil, fmt.Errorf("store: stats read pipelines: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: stats scan pipeline: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stats read pipelines: %w", err)
	}
	return out, nil
}

// Journal reports the data-journal counters as zero: their rows live in the data
// database and their reader machinery is E07's, not yet landed. Reporting zero is
// the JournalStats seam's documented default -- honest absence, never a fabricated
// count. It never errors.
func (s *pgxStatsSource) Journal(context.Context) (JournalStats, error) {
	return JournalStats{}, nil
}

// Checkpoints reads the checkpoint chain rows: insert-order identity (seq), the
// chained digest (hex-encoded), and the sealed partition's location.
func (s *pgxStatsSource) Checkpoints(ctx context.Context) ([]Checkpoint, error) {
	rows, err := s.pool.query(ctx, statsCheckpointsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: stats read checkpoints: %w", err)
	}
	defer rows.Close()

	var out []Checkpoint
	for rows.Next() {
		var cp Checkpoint
		var digest []byte
		if err := rows.Scan(&cp.Seq, &digest, &cp.Location); err != nil {
			return nil, fmt.Errorf("store: stats scan checkpoint: %w", err)
		}
		cp.Digest = hex.EncodeToString(digest)
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stats read checkpoints: %w", err)
	}
	return out, nil
}
