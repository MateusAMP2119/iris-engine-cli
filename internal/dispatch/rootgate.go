package dispatch

import "github.com/MateusAMP2119/iris-lakehouse/internal/store"

// This file is the root cause gate: the loop-pass eligibility decision for a
// pipeline with no depends_on edges. It closes the gate model's symmetry -- a
// dependent's run consumes upstream runs (run_inputs, 1:1), and a root's
// cause=loop run consumes its declaration -- so no loop run ever starts without
// an unconsumed cause. Before this gate, a root was eligible every pass and
// re-ran back to back forever, the busy loop issue #172 diagnoses.
//
// The decision is derived, never stored: it compares the pipeline's current
// declaration checksum against the one stamped on its latest run
// (runs.declaration_checksum, recorded on every run), the same
// derived-not-stored shape as the gate's run_inputs consumed check. Manual runs,
// replays, and wipes need no case here: each produces or removes run rows, so
// the latest-run read reflects them (a manual run of the current declaration
// consumes it; a wipe that deletes the runs leaves the declaration unconsumed
// again). The manual path itself is untouched -- an operator run is its own
// cause and never consults this gate.

// RootRun is a root pipeline's latest run as the root cause gate reads it: its
// lifecycle state and the declaration checksum stamped when it was minted. Nil
// (no run at all) means the registration itself is the unconsumed cause.
type RootRun struct {
	// State is the run's lifecycle state.
	State store.RunState
	// DeclarationChecksum is the declaration hash stamped on the run
	// (runs.declaration_checksum).
	DeclarationChecksum string
}

// DecideRoot resolves a root (edge-less) pipeline's loop-pass eligibility from
// its latest run and its current declaration checksum. It is pure -- no I/O --
// and a function of its inputs alone.
//
// The root runs when an unconsumed cause exists: no run at all (a fresh or
// re-registered pipeline, or one whose runs a wipe removed), or a latest run
// whose stamped declaration checksum differs from the current one (the
// declaration changed since it last ran, whatever that run's cause or outcome).
// A latest run still queued or running parks the decision until it reaches a
// terminal state -- its terminal transition re-opens this check. A terminal run
// stamped with the current checksum means the declaration is consumed: the root
// parks, and a dead-lettered run parks exactly the same way, because a failed
// run is never retried on its own -- re-execution is only ever an explicit
// replay (clock doctrine's no-retry rule, unchanged).
func DecideRoot(latest *RootRun, currentChecksum string) Decision {
	if latest == nil {
		// No run at all: the registration is the unconsumed cause.
		return Decision{Run: true}
	}
	if latest.State == store.RunQueued || latest.State == store.RunRunning {
		// A run is already in flight: wait for its terminal transition.
		return Decision{}
	}
	if latest.DeclarationChecksum != currentChecksum {
		// The declaration changed since the latest run: a new unconsumed cause.
		return Decision{Run: true}
	}
	// The current declaration is consumed by a terminal run (succeeded or
	// dead-lettered alike): park. Manual run and replay remain the operator's
	// re-execution surfaces.
	return Decision{}
}
