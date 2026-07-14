package dispatch

// This file is the pure core of startup reconciliation (crash recovery). A leader --
// cold start or failover, the same code path -- runs reconciliation before
// dispatching any lane: it disposes of the run records a
// crashed or deposed predecessor left behind. Leftover *running* runs are
// dead-lettered (reason stopped, "daemon terminated while run was in flight");
// *queued* never-started runs are deleted (they consumed nothing, so the next pass
// recreates them); and surviving process groups are best-effort SIGKILLed from
// their recorded handles first, but only on the host that spawned them.
//
// Reconcile is deliberately pure: it reads no meta, spawns no kill, and writes
// nothing. It maps a view of run records to an ordered Plan -- the kills that must
// happen first, then the disposals -- which the Reconciler (reconciler.go) applies.
// This split keeps the whole decision testable on table fixtures with no I/O, and
// keeps this file's imports down to the meta store alone: it imports nothing
// journal-related (the journal lives in the internal/pg data client, never touched
// here), the mechanism the no-journal-touch contract asserts.

import "github.com/MateusAMP2119/iris-engine-cli/internal/store"

// DaemonTerminatedDetail is the human error detail recorded (in dead_letters.error)
// for a run left in flight when the daemon terminated. The dead-letter *reason* enum
// is store.ReasonStopped; this string is the detail beside it (crash recovery:
// 'dead-lettered (stopped, "daemon terminated while run was in flight")').
const DaemonTerminatedDetail = "daemon terminated while run was in flight"

// ActionKind is the kind of one reconciliation action. The set is closed and small --
// kill, dead-letter, delete-queued -- and there is deliberately no journal action of
// any kind: reconciliation never reads, writes, or replays the journal (crash
// recovery: "No journal step"). AllActionKinds enumerates the whole vocabulary so a
// test can prove the absence of a journal action.
type ActionKind int

// The reconciliation action kinds -- the complete vocabulary.
const (
	// ActionKill SIGKILLs a surviving process group by its recorded handle.
	ActionKill ActionKind = iota
	// ActionDeadLetter dead-letters a leftover running run.
	ActionDeadLetter
	// ActionDeleteQueued deletes a queued never-started run.
	ActionDeleteQueued
)

// String names the action kind.
func (k ActionKind) String() string {
	switch k {
	case ActionKill:
		return "kill"
	case ActionDeadLetter:
		return "deadletter"
	case ActionDeleteQueued:
		return "delete-queued"
	default:
		return "unknown"
	}
}

// AllActionKinds returns reconciliation's complete action vocabulary, in a stable
// order. It is the closed set: kill a survivor, dead-letter a running run, delete a
// queued run. There is no journal, replay, wipe, or promotion action -- crash
// reconciliation never touches the journal.
func AllActionKinds() []ActionKind {
	return []ActionKind{ActionKill, ActionDeadLetter, ActionDeleteQueued}
}

// HostMatcher reports whether a run's recorded handle (its process-group id) was
// spawned on this host, so its process group is meaningful and killable here. A
// handle is only valid on the host that spawned it: same-host restart kills the
// survivors from their handles; a cross-host failover leader must NOT kill (the
// deposed leader's self-demotion does that -- E11), it only dead-letters. Today
// the engine is single-host, so SingleHostMatcher (always true) is wired; E11
// injects the real cross-host discriminator through this same seam.
type HostMatcher func(run store.Run) bool

// SingleHostMatcher is the single-host host-identity predicate: every recorded
// handle was spawned here, so every survivor is killable. It is the matcher wired
// today; E11 replaces it with a real this-host discriminator without touching the
// reconciliation core.
func SingleHostMatcher() HostMatcher {
	return func(store.Run) bool { return true }
}

// ReconcileView is the input to the pure reconciliation core: the leftover run
// records a restarting leader finds in meta (the non-terminal ones -- running and
// queued), plus the host-identity predicate that decides which survivors are
// killable here.
type ReconcileView struct {
	// Runs are the run records to reconcile. Terminal runs (succeeded, dead-lettered)
	// among them are ignored; only running and queued runs produce actions.
	Runs []store.Run
	// SpawnedHere reports whether a run's recorded handle is killable on this host. A
	// nil predicate is treated as single-host (SingleHostMatcher).
	SpawnedHere HostMatcher
}

// KillAction names a surviving process group to SIGKILL by its recorded handle
// (pgid), before its run is disposed of.
type KillAction struct {
	// RunID is the run whose process group survived.
	RunID string
	// Handle is the recorded process-group id (runs.handle) to SIGKILL.
	Handle int
}

// DisposalAction disposes of one leftover run: dead-letter a running run
// (ActionDeadLetter) or delete a queued never-started run (ActionDeleteQueued).
type DisposalAction struct {
	// Kind is ActionDeadLetter or ActionDeleteQueued.
	Kind ActionKind
	// RunID is the run being disposed of.
	RunID string
	// Reason is the dead-letter reason, set for ActionDeadLetter (ReasonStopped).
	Reason store.DeadLetterReason
	// Detail is the human error detail, set for ActionDeadLetter
	// (DaemonTerminatedDetail).
	Detail string
}

// Plan is the ordered reconciliation plan: every kill happens before any disposal
// (crash recovery: survivors are SIGKILLed "first"). The Reconciler runs all Kills,
// then all Disposals, so the kill-before-disposal order is structural.
type Plan struct {
	// Kills are the surviving process groups to SIGKILL, before any disposal.
	Kills []KillAction
	// Disposals are the run dispositions (dead-letter, delete), after every kill.
	Disposals []DisposalAction
}

// Reconcile computes the reconciliation Plan from a view of leftover run records. It
// is pure: no meta reads, no kills, no writes. For each run it finds:
//
//   - a running run is dead-lettered (ActionDeadLetter, reason stopped, detail
//     DaemonTerminatedDetail); and if it has a recorded handle that was spawned on
//     this host, that process group is SIGKILLed first (ActionKill);
//   - a queued never-started run is deleted (ActionDeleteQueued);
//   - a terminal run (succeeded, dead-lettered) is left untouched.
//
// Every kill lands in Plan.Kills and every disposal in Plan.Disposals, so the
// Reconciler applies all kills before any disposal.
func Reconcile(view ReconcileView) Plan {
	matcher := view.SpawnedHere
	if matcher == nil {
		matcher = SingleHostMatcher()
	}
	var plan Plan
	for _, run := range view.Runs {
		switch run.State {
		case store.RunRunning:
			// A same-host survivor with a recorded handle: SIGKILL its process group,
			// as a Kill (all kills precede all disposals). A run with no recorded handle
			// (crashed before its handle was written) or one spawned on another host
			// yields no kill -- the deposed leader kills its own survivors (E11).
			if run.Handle != 0 && matcher(run) {
				plan.Kills = append(plan.Kills, KillAction{RunID: run.ID, Handle: run.Handle})
			}
			// Always dead-letter the leftover running run: stopped, daemon-terminated.
			plan.Disposals = append(plan.Disposals, DisposalAction{
				Kind:   ActionDeadLetter,
				RunID:  run.ID,
				Reason: store.ReasonStopped,
				Detail: DaemonTerminatedDetail,
			})
		case store.RunQueued:
			// A queued never-started run consumed nothing: delete it, so the next
			// dispatch pass recreates it.
			plan.Disposals = append(plan.Disposals, DisposalAction{
				Kind:  ActionDeleteQueued,
				RunID: run.ID,
			})
		default:
			// Terminal runs (succeeded, dead-lettered) are already settled: untouched.
		}
	}
	return plan
}
