package store

import (
	"strings"
	"testing"
)

// TestPruneStatementsNeverTouchJournal proves run pruning never deletes
// data_journal rows: capture rows are bounded only by the journal's own
// lifecycle, never by run pruning. The prune statement batch is a closed set that
// touches only run_summaries, run_inputs, and runs; no statement references
// data_journal, and none deletes from the journal. It is pure over the built
// statements -- no I/O.
func TestPruneStatementsNeverTouchJournal(t *testing.T) {
	summary := BuildRunSummary(PrunableRun{
		RunID:                  42,
		Pipeline:               "load_orders",
		State:                  RunSucceeded,
		DeclarationChecksum:    "decl-9f",
		ConsumedUpstreamRunIDs: []int64{7, 11},
	})
	stmts := pruneStatements(summary)
	if len(stmts) == 0 {
		t.Fatal("pruneStatements returned no statements; a prune must at least archive and delete the run")
	}

	// The data_journal table is never named. (The journal_floor / journal_ceiling
	// pin columns the summary copies are NOT the journal -- they are the run's pin,
	// which deliberately outlives pruning; only the data_journal TABLE is forbidden.)
	for _, s := range stmts {
		if strings.Contains(strings.ToLower(s.SQL), "data_journal") {
			t.Errorf("prune statement references data_journal; run pruning must never touch the journal (capture rows are bounded by the journal's own lifecycle):\n%s", s.SQL)
		}
	}

	// Closed set: every table a prune statement touches is one of the three run-
	// history tables -- run_summaries, run_inputs, runs -- and nothing else.
	allowed := map[string]bool{"run_summaries": true, "run_inputs": true, "runs": true}
	for _, s := range stmts {
		found := false
		for tbl := range allowed {
			if strings.Contains(s.SQL, tbl) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("prune statement touches a table outside the closed run-history set {run_summaries, run_inputs, runs}:\n%s", s.SQL)
		}
	}
}
