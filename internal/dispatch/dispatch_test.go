package dispatch_test

import (
	"bytes"
	"context"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// recordingWriteConn is a store.MetaWriteConn that records every write it is
// handed, so a test can prove all meta writes arrived through the dispatcher.
type recordingWriteConn struct {
	mu    sync.Mutex
	stmts []string
}

func (c *recordingWriteConn) Exec(_ context.Context, sql string, _ ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = append(c.stmts, sql)
	return nil
}

func (c *recordingWriteConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stmts)
}

// goroutineID returns the current goroutine's runtime id, so the test can prove
// every submitted write executed on the one dispatcher goroutine.
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine <id> [running]:"
	fields := bytes.Fields(buf[:n])
	id, _ := strconv.ParseUint(string(fields[1]), 10, 64)
	return id
}

// TestDispatcherSoleMetaWriter proves the single-writer meta path: every meta write
// is submitted to one Dispatcher and executed serially on its one goroutine, so no
// two writes overlap and no write reaches meta except through the dispatcher
// (specification section 6.1: the dispatcher is a single goroutine, the sole meta
// writer).
//
// spec: S06.1/dispatcher-sole-meta-writer
func TestDispatcherSoleMetaWriter(t *testing.T) {
	t.Run("S06.1/dispatcher-sole-meta-writer", func(t *testing.T) {
		conn := &recordingWriteConn{}
		d := dispatch.New(conn)
		ctx := context.Background()
		d.Start(ctx)
		defer d.Stop()

		const writers = 32
		var (
			active   int32 // in-flight writes; must never exceed 1 under serialization
			overlaps int32
			ids      sync.Map // distinct goroutine ids the writes ran on
			wg       sync.WaitGroup
		)
		wg.Add(writers)
		for i := 0; i < writers; i++ {
			go func() {
				defer wg.Done()
				err := d.Submit(ctx, func(w *store.Writer) error {
					if atomic.AddInt32(&active, 1) != 1 {
						atomic.AddInt32(&overlaps, 1)
					}
					ids.Store(goroutineID(), true)
					// A real leader-only meta write: the schema re-check.
					err := w.EnsureSchema(ctx)
					atomic.AddInt32(&active, -1)
					return err
				})
				if err != nil {
					t.Errorf("Submit: %v", err)
				}
			}()
		}
		wg.Wait()

		if overlaps != 0 {
			t.Errorf("%d write(s) overlapped; the dispatcher must serialize every meta write", overlaps)
		}
		distinct := 0
		ids.Range(func(_, _ any) bool { distinct++; return true })
		if distinct != 1 {
			t.Errorf("writes ran on %d goroutines, want exactly 1 (the sole dispatcher goroutine)", distinct)
		}
		// Every write reached meta, and only through the dispatcher: the schema
		// re-check issues len(DDL) statements per submission.
		wantStmts := writers * len(store.MetaSchema().DDL())
		if got := conn.count(); got != wantStmts {
			t.Errorf("meta received %d statements, want %d (all via the dispatcher)", got, wantStmts)
		}
	})

	t.Run("Submit after Stop is rejected, not silently dropped", func(t *testing.T) {
		d := dispatch.New(&recordingWriteConn{})
		d.Start(context.Background())
		d.Stop()
		err := d.Submit(context.Background(), func(*store.Writer) error { return nil })
		if err == nil {
			t.Error("Submit after Stop returned nil; a stopped dispatcher must reject writes")
		}
	})
}
