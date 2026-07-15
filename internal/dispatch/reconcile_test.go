package dispatch_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// runningRun is a leftover running run fixture with a recorded handle (pgid).
func runningRun(id string, pgid int) store.Run {
	return store.Run{ID: id, Pipeline: "p", Lane: "l", State: store.RunRunning, Handle: pgid}
}

// queuedRun is a queued never-started run fixture.
func queuedRun(id string) store.Run {
	return store.Run{ID: id, Pipeline: "p", Lane: "l", State: store.RunQueued}
}

// deadLetters returns the dead-letter disposals in a plan.
func deadLetters(p dispatch.Plan) []dispatch.DisposalAction {
	var out []dispatch.DisposalAction
	for _, d := range p.Disposals {
		if d.Kind == dispatch.ActionDeadLetter {
			out = append(out, d)
		}
	}
	return out
}

// deletes returns the delete-queued disposals in a plan.
func deletes(p dispatch.Plan) []dispatch.DisposalAction {
	var out []dispatch.DisposalAction
	for _, d := range p.Disposals {
		if d.Kind == dispatch.ActionDeleteQueued {
			out = append(out, d)
		}
	}
	return out
}

// TestReconcileInflightRunsDeadLettered proves the pure reconciliation core
// dead-letters every leftover running run with reason stopped and the daemon-
// terminated detail, and never deletes one (crash recovery: leftover running runs are
// dead-lettered, "daemon terminated while run was in flight"; the enum is stopped,
// the string is the error detail).
func TestReconcileInflightRunsDeadLettered(t *testing.T) {
	t.Run("inflight-runs-deadlettered", func(t *testing.T) {
		view := dispatch.ReconcileView{
			Runs:        []store.Run{runningRun("run-1", 4242), runningRun("run-2", 0)},
			SpawnedHere: dispatch.SingleHostMatcher(),
		}
		plan := dispatch.Reconcile(view)

		dls := deadLetters(plan)
		if len(dls) != 2 {
			t.Fatalf("dead-letters = %d, want 2 (both running runs dead-lettered): %+v", len(dls), plan.Disposals)
		}
		if len(deletes(plan)) != 0 {
			t.Errorf("a running run was deleted; leftover running runs are dead-lettered, never deleted: %+v", plan.Disposals)
		}
		for _, dl := range dls {
			if dl.Reason != store.ReasonStopped {
				t.Errorf("run %s dead-letter reason = %q, want %q (the stopped enum value)", dl.RunID, dl.Reason, store.ReasonStopped)
			}
			if dl.Detail != dispatch.DaemonTerminatedDetail {
				t.Errorf("run %s dead-letter detail = %q, want %q", dl.RunID, dl.Detail, dispatch.DaemonTerminatedDetail)
			}
			if dl.Detail != "daemon terminated while run was in flight" {
				t.Errorf("run %s detail string drifted from the required exact wording: %q", dl.RunID, dl.Detail)
			}
		}
	})
}

// TestReconcileQueuedRunsDeleted proves queued never-started runs are deleted, not
// dead-lettered, so the next dispatch pass recreates them (crash recovery: "Queued
// never-started runs: deleted, not dead-lettered"). Terminal runs among the fixtures
// are left untouched.
func TestReconcileQueuedRunsDeleted(t *testing.T) {
	t.Run("queued-runs-deleted", func(t *testing.T) {
		view := dispatch.ReconcileView{
			Runs: []store.Run{
				queuedRun("run-1"),
				queuedRun("run-2"),
				{ID: "run-3", State: store.RunSucceeded},
				{ID: "run-4", State: store.RunDeadLettered},
			},
			SpawnedHere: dispatch.SingleHostMatcher(),
		}
		plan := dispatch.Reconcile(view)

		del := deletes(plan)
		if len(del) != 2 {
			t.Fatalf("delete-queued actions = %d, want 2: %+v", len(del), plan.Disposals)
		}
		gotIDs := map[string]bool{}
		for _, d := range del {
			gotIDs[d.RunID] = true
		}
		if !gotIDs["run-1"] || !gotIDs["run-2"] {
			t.Errorf("deleted run ids = %v, want run-1 and run-2", gotIDs)
		}
		if len(deadLetters(plan)) != 0 {
			t.Errorf("a queued run was dead-lettered; queued never-started runs are deleted, not dead-lettered: %+v", plan.Disposals)
		}
		if len(plan.Kills) != 0 {
			t.Errorf("a queued run yielded a kill; queued runs never started a process group: %+v", plan.Kills)
		}
		// Terminal runs (succeeded, dead-lettered) produced no action at all.
		if got := len(plan.Disposals); got != 2 {
			t.Errorf("disposals = %d, want 2; terminal runs (succeeded, dead-lettered) must be untouched", got)
		}
	})
}

// TestReconcileNoJournalTouch proves reconciliation performs no journal step (crash
// recovery: "No journal step"). Two mechanisms: the action vocabulary is a closed set
// of kill/dead-letter/delete with no journal (replay, wipe, promotion) action of any
// kind; and the pure-core source file imports nothing journal-related -- the journal
// lives in the internal/pg data client (public.data_journal in the data database),
// which reconcile.go never imports.
func TestReconcileNoJournalTouch(t *testing.T) {
	t.Run("reconcile-no-journal-touch", func(t *testing.T) {
		t.Run("the action vocabulary has no journal action", func(t *testing.T) {
			kinds := dispatch.AllActionKinds()
			want := []dispatch.ActionKind{dispatch.ActionKill, dispatch.ActionDeadLetter, dispatch.ActionDeleteQueued}
			if len(kinds) != len(want) {
				t.Fatalf("action vocabulary = %v, want exactly %v (kill, dead-letter, delete -- no journal action)", kinds, want)
			}
			for i := range want {
				if kinds[i] != want[i] {
					t.Errorf("action[%d] = %q, want %q", i, kinds[i], want[i])
				}
			}
			// No action names a journal operation (replay, wipe, promote, revert).
			for _, k := range kinds {
				name := k.String()
				for _, banned := range []string{"journal", "replay", "wipe", "promote", "revert"} {
					if strings.Contains(name, banned) {
						t.Errorf("action %q names a journal operation %q; reconciliation performs no journal step", name, banned)
					}
				}
			}
		})

		t.Run("a rich fixture yields only kill/dead-letter/delete actions", func(t *testing.T) {
			view := dispatch.ReconcileView{
				Runs: []store.Run{
					runningRun("run-1", 100),
					queuedRun("run-2"),
					{ID: "run-3", State: store.RunSucceeded},
				},
				SpawnedHere: dispatch.SingleHostMatcher(),
			}
			plan := dispatch.Reconcile(view)
			for _, k := range plan.Kills {
				_ = k // Kills are, by type, ActionKill only.
			}
			for _, d := range plan.Disposals {
				if d.Kind != dispatch.ActionDeadLetter && d.Kind != dispatch.ActionDeleteQueued {
					t.Errorf("disposal kind %q is not a dead-letter or delete; reconciliation disposes runs only two ways", d.Kind)
				}
			}
		})

		t.Run("the reconcile core imports nothing journal-related", func(t *testing.T) {
			// The journal is public.data_journal in the DATA database, reached only
			// through the internal/pg data client. Reconciliation touches meta and the
			// kill seam, never the journal, so its pure-core source imports neither the
			// pg data client nor any journal-named package. Parsing the file's own
			// imports proves the absence structurally.
			const coreFile = "reconcile.go"
			src, err := os.ReadFile(coreFile)
			if err != nil {
				t.Fatalf("read %s: %v", coreFile, err)
			}
			f, err := parser.ParseFile(token.NewFileSet(), coreFile, src, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", coreFile, err)
			}
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if strings.HasSuffix(path, "/internal/pg") || strings.Contains(path, "/internal/pg/") {
					t.Errorf("%s imports the pg data client (%q); the journal lives there and reconciliation never touches it", coreFile, path)
				}
				if strings.Contains(strings.ToLower(path), "journal") {
					t.Errorf("%s imports a journal-related package (%q); reconciliation performs no journal step", coreFile, path)
				}
			}
		})
	})
}
