package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
)

// lockedBuffer is a concurrency-safe capture buffer standing in for a per-run log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestProtocolScanner(t *testing.T) {
	t.Run("protocol-scanner", func(t *testing.T) {
		t.Run("done line consumed, log lines forwarded, chunked writes joined", func(t *testing.T) {
			sink := &lockedBuffer{}
			s := newProtocolScanner(sink)
			for _, chunk := range []string{"hel", "lo\ndo", "ne 0\nrest\n"} {
				if _, err := s.Write([]byte(chunk)); err != nil {
					t.Fatalf("scanner write: %v", err)
				}
			}
			select {
			case ev := <-s.done:
				if ev.code != 0 {
					t.Fatalf("done code = %d, want 0", ev.code)
				}
			default:
				t.Fatal("done line was not parsed")
			}
			if got := sink.String(); got != "hello\nrest\n" {
				t.Fatalf("forwarded log = %q, want the non-protocol lines only", got)
			}
		})

		t.Run("near-miss lines are log output, not protocol", func(t *testing.T) {
			sink := &lockedBuffer{}
			s := newProtocolScanner(sink)
			_, _ = s.Write([]byte("done\ndone x\ndone 1 2\nwell done 0\n"))
			select {
			case ev := <-s.done:
				t.Fatalf("parsed a done event %v from a near-miss line", ev)
			default:
			}
			if got := sink.String(); !strings.Contains(got, "well done 0") {
				t.Fatalf("near-miss lines missing from log: %q", got)
			}
		})

		t.Run("second done for one iteration is dropped", func(t *testing.T) {
			s := newProtocolScanner(&lockedBuffer{})
			_, _ = s.Write([]byte("done 0\ndone 1\n"))
			if ev := <-s.done; ev.code != 0 {
				t.Fatalf("first done = %d, want 0", ev.code)
			}
			select {
			case ev := <-s.done:
				t.Fatalf("second done %v must be dropped", ev)
			default:
			}
		})
	})
}

func TestSwitchSink(t *testing.T) {
	t.Run("switch-sink", func(t *testing.T) {
		s := &switchSink{}
		if n, err := s.Write([]byte("dropped")); n != 7 || err != nil {
			t.Fatalf("nil-destination write = (%d, %v), want best-effort discard", n, err)
		}
		buf := &lockedBuffer{}
		s.Set(buf)
		_, _ = s.Write([]byte("kept"))
		s.Set(nil)
		_, _ = s.Write([]byte("between-iterations"))
		if got := buf.String(); got != "kept" {
			t.Fatalf("sink captured %q, want only the current iteration's output", got)
		}
	})
}

// writeScript drops an executable shell script into dir and returns its argv.
func writeScript(t *testing.T, dir, body string) []string {
	t.Helper()
	path := filepath.Join(dir, "main.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // test script must be executable
		t.Fatalf("write script: %v", err)
	}
	return []string{"/bin/sh", path}
}

// waitDone receives one done event or fails after a deadline.
func waitDone(t *testing.T, s *residentSession) doneEvent {
	t.Helper()
	select {
	case ev := <-s.scanner.done:
		return ev
	case <-s.exited:
		t.Fatalf("process exited (status %+v) instead of answering done", s.status)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a done answer")
	}
	return doneEvent{}
}

func TestResidentSessionRealProcess(t *testing.T) {
	t.Run("resident-session-real-process", func(t *testing.T) {
		ctx := context.Background()
		runner := exec.NewOSRunner()

		t.Run("one process iterates in place and exits on stdin EOF", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, `while read verb rid; do
  [ "$verb" = "go" ] || continue
  echo "iter $rid"
  echo "done 0"
done
`)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			pgid := ses.handle.PGID()

			first := &lockedBuffer{}
			ses.out.Set(first)
			if err := ses.sendGo(1); err != nil {
				t.Fatalf("sendGo(1): %v", err)
			}
			if ev := waitDone(t, ses); ev.code != 0 {
				t.Fatalf("iteration 1 status = %d, want 0", ev.code)
			}
			ses.out.Set(nil)

			second := &lockedBuffer{}
			ses.out.Set(second)
			if err := ses.sendGo(2); err != nil {
				t.Fatalf("sendGo(2): %v", err)
			}
			if ev := waitDone(t, ses); ev.code != 0 {
				t.Fatalf("iteration 2 status = %d, want 0", ev.code)
			}

			if ses.dead() {
				t.Fatal("session died between iterations; the process must stay resident")
			}
			if got := ses.handle.PGID(); got != pgid {
				t.Fatalf("pgid changed across iterations: %d -> %d", pgid, got)
			}
			if got := first.String(); !strings.Contains(got, "iter 1") || strings.Contains(got, "iter 2") {
				t.Fatalf("first iteration log = %q, want only its own output", got)
			}
			if got := second.String(); !strings.Contains(got, "iter 2") || strings.Contains(got, "iter 1") {
				t.Fatalf("second iteration log = %q, want only its own output", got)
			}

			ses.end()
			if !ses.dead() {
				t.Fatal("end() must reap the process")
			}
		})

		t.Run("non-zero done status reports without ending the process", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, `while read verb rid; do
  echo "done 7"
done
`)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			defer ses.end()
			if err := ses.sendGo(1); err != nil {
				t.Fatalf("sendGo: %v", err)
			}
			if ev := waitDone(t, ses); ev.code != 7 {
				t.Fatalf("status = %d, want 7", ev.code)
			}
			if ses.dead() {
				t.Fatal("a failed iteration must not end the resident process")
			}
		})

		t.Run("legacy script ignores the protocol and reports through exit", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, `echo "plain output"
exit 3
`)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			sink := &lockedBuffer{}
			ses.out.Set(sink)
			_ = ses.sendGo(1)
			select {
			case <-ses.exited:
			case <-time.After(10 * time.Second):
				t.Fatal("legacy process did not exit")
			}
			if ses.status.Code != 3 {
				t.Fatalf("exit code = %d, want 3", ses.status.Code)
			}
			if got := sink.String(); !strings.Contains(got, "plain output") {
				t.Fatalf("legacy output not captured: %q", got)
			}
		})
	})
}
