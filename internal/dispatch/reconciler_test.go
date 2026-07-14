package dispatch_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// actionLog is a shared, ordered record of the reconciler's side effects across two
// fakes -- the group killer and the meta write connection -- so a test can assert
// the global order (every kill before every disposal write) rather than each fake's
// order in isolation.
type actionLog struct {
	mu     sync.Mutex
	events []string
}

func (l *actionLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *actionLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// recordingKiller is a fake dispatch.GroupKiller: it records the pgid of every
// process group it is asked to SIGKILL, in the shared action log and its own list,
// with no real process.
type recordingKiller struct {
	log  *actionLog
	mu   sync.Mutex
	pgid []int
}

func (k *recordingKiller) KillGroup(pgid int) error {
	k.log.add(fmt.Sprintf("kill:%d", pgid))
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pgid = append(k.pgid, pgid)
	return nil
}

func (k *recordingKiller) killed() []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]int(nil), k.pgid...)
}

// metaWrite is one recorded meta write: its SQL and arguments.
type metaWrite struct {
	sql  string
	args []any
}

// recordingWriteConnArgs is a store.MetaWriteConn that records every write (SQL and
// args) in the shared action log and its own list, so a test can assert both the
// order relative to kills and the exact disposal statements.
type recordingWriteConnArgs struct {
	log    *actionLog
	mu     sync.Mutex
	writes []metaWrite
}

func (c *recordingWriteConnArgs) Exec(_ context.Context, sql string, args ...any) error {
	c.log.add("write:" + sql)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, metaWrite{sql: sql, args: args})
	return nil
}

func (c *recordingWriteConnArgs) recorded() []metaWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]metaWrite(nil), c.writes...)
}

// seedRunning creates a queued run in the fake and transitions it to running with
// the given recorded handle (pgid); a zero handle models a run that crashed before
// its handle was recorded.
func seedRunning(t *testing.T, f *storetest.Fake, pgid int) store.Run {
	t.Helper()
	ctx := context.Background()
	r, err := f.CreateRun(ctx, store.RunSpec{Pipeline: "p", Lane: "l"})
	if err != nil {
		t.Fatalf("seed CreateRun: %v", err)
	}
	updates := []store.RunUpdate{}
	if pgid != 0 {
		updates = append(updates, store.WithHandle(pgid))
	}
	if _, err := f.SetRunState(ctx, r.ID, store.RunRunning, updates...); err != nil {
		t.Fatalf("seed SetRunState running: %v", err)
	}
	return r
}

// seedQueued creates a queued never-started run in the fake.
func seedQueued(t *testing.T, f *storetest.Fake) store.Run {
	t.Helper()
	r, err := f.CreateRun(context.Background(), store.RunSpec{Pipeline: "p", Lane: "l"})
	if err != nil {
		t.Fatalf("seed CreateRun: %v", err)
	}
	return r
}

// findDeadLetter returns the atomic dead-letter statement recorded for run id, if
// any. The dead-letter is one CTE (UPDATE runs ... INSERT INTO dead_letters ...), so
// it is matched by its INSERT clause; its args are
// (RunDeadLettered, id, RunRunning, reason, detail), so the run id is args[1].
func findDeadLetter(writes []metaWrite, id string) (metaWrite, bool) {
	for _, w := range writes {
		if strings.Contains(w.sql, "INSERT INTO dead_letters") && len(w.args) >= 2 && w.args[1] == id {
			return w, true
		}
	}
	return metaWrite{}, false
}

// TestReconcilerSameHostRestartKills proves same-host restart reconciliation
// best-effort SIGKILLs surviving process groups from their recorded handles BEFORE
// disposing of their runs, and that a cross-host failover leader dead-letters
// without killing. It drives the real Reconciler over the real single-writer
// Dispatcher, with a fake reader, a fake group killer, and a recording write
// connection -- no real process, no live Postgres. The recorded run handle
// (runs.handle = the process-group id) is the crash-recovery key the restart SIGKILLs
// each survivor by: a run with a recorded handle is killed by exactly that pgid, one
// with no handle is not.
func TestReconcilerSameHostRestartKills(t *testing.T) {
	t.Run("samehost-restart-kills", func(t *testing.T) {
		t.Run("same-host restart kills survivors from recorded handles before disposal", func(t *testing.T) {
			ctx := context.Background()
			log := &actionLog{}
			f := storetest.New()
			survivor := seedRunning(t, f, 5001) // running, recorded handle 5001
			noHandle := seedRunning(t, f, 0)    // running, crashed before handle recorded
			queued := seedQueued(t, f)          // queued never-started

			conn := &recordingWriteConnArgs{log: log}
			d := dispatch.New(conn)
			d.Start(ctx)
			defer d.Stop()
			killer := &recordingKiller{log: log}

			rec := dispatch.NewReconciler(f, d, killer, dispatch.SingleHostMatcher(), nil)
			if err := rec.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			// The survivor's recorded process group was SIGKILLed; the handle-less run
			// yielded no kill (there is no group to signal).
			if got := killer.killed(); len(got) != 1 || got[0] != 5001 {
				t.Fatalf("killed pgids = %v, want exactly [5001] (survivor's recorded handle)", got)
			}

			// KILL precedes disposal: every kill event comes before every disposal write.
			events := log.snapshot()
			lastKill, firstWrite := -1, len(events)
			for i, e := range events {
				switch {
				case strings.HasPrefix(e, "kill:") && i > lastKill:
					lastKill = i
				case strings.HasPrefix(e, "write:") && i < firstWrite:
					firstWrite = i
				}
			}
			if lastKill == -1 {
				t.Fatalf("no kill was recorded; a same-host survivor must be SIGKILLed: %v", events)
			}
			if firstWrite == len(events) {
				t.Fatalf("no disposal write was recorded: %v", events)
			}
			if lastKill > firstWrite {
				t.Errorf("a kill (index %d) followed a disposal write (index %d); survivors are SIGKILLed BEFORE their runs are disposed of: %v", lastKill, firstWrite, events)
			}

			// Both running runs were dead-lettered (stopped, daemon-terminated); the
			// queued run was deleted.
			writes := conn.recorded()
			for _, id := range []string{survivor.ID, noHandle.ID} {
				dl, ok := findDeadLetter(writes, id)
				if !ok {
					t.Fatalf("no dead_letters row recorded for running run %s: %v", id, writes)
				}
				// args: (RunDeadLettered, id, RunRunning, reason, detail).
				if len(dl.args) != 5 {
					t.Fatalf("atomic dead-letter of run %s has %d args, want 5 (RunDeadLettered, id, RunRunning, reason, detail): %v", id, len(dl.args), dl.args)
				}
				if dl.args[3] != store.ReasonStopped {
					t.Errorf("run %s dead-letter reason arg = %v, want %v", id, dl.args[3], store.ReasonStopped)
				}
				if dl.args[4] != dispatch.DaemonTerminatedDetail {
					t.Errorf("run %s dead-letter detail arg = %v, want %q", id, dl.args[4], dispatch.DaemonTerminatedDetail)
				}
			}
			if !hasDelete(writes, queued.ID) {
				t.Errorf("no DELETE recorded for queued run %s: %v", queued.ID, writes)
			}
		})

		t.Run("cross-host failover dead-letters without killing", func(t *testing.T) {
			ctx := context.Background()
			log := &actionLog{}
			f := storetest.New()
			survivor := seedRunning(t, f, 7001)

			conn := &recordingWriteConnArgs{log: log}
			d := dispatch.New(conn)
			d.Start(ctx)
			defer d.Stop()
			killer := &recordingKiller{log: log}

			// A cross-host matcher: the handle was NOT spawned on this host, so its
			// process group is not killable here (E11 supplies the real discriminator).
			crossHost := dispatch.HostMatcher(func(store.Run) bool { return false })
			rec := dispatch.NewReconciler(f, d, killer, crossHost, nil)
			if err := rec.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if got := killer.killed(); len(got) != 0 {
				t.Errorf("cross-host reconciliation SIGKILLed %v; it must dead-letter without killing (the deposed leader kills its own survivors)", got)
			}
			if _, ok := findDeadLetter(conn.recorded(), survivor.ID); !ok {
				t.Errorf("cross-host reconciliation did not dead-letter the leftover running run %s", survivor.ID)
			}
		})
	})
}

// hasDelete reports whether a DELETE for the queued run id was recorded.
func hasDelete(writes []metaWrite, id string) bool {
	for _, w := range writes {
		if strings.HasPrefix(w.sql, "DELETE FROM runs") && len(w.args) > 0 && w.args[0] == id {
			return true
		}
	}
	return false
}
