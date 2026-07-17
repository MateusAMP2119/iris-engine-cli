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

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
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

func TestFrameScanner(t *testing.T) {
	t.Run("frame-scanner", func(t *testing.T) {
		t.Run("chunked writes join to whole lines", func(t *testing.T) {
			s := newFrameScanner()
			for _, chunk := range []string{`{"event":"do`, "ne\",\"turn\":1}\n", `{"event":"row"}` + "\n"} {
				if _, err := s.Write([]byte(chunk)); err != nil {
					t.Fatalf("scanner write: %v", err)
				}
			}
			var lines []string
			for len(s.lines) > 0 {
				lines = append(lines, <-s.lines)
			}
			if len(lines) != 2 || lines[0] != `{"event":"done","turn":1}` || lines[1] != `{"event":"row"}` {
				t.Fatalf("delivered lines = %q", lines)
			}
		})

		t.Run("carriage returns are trimmed", func(t *testing.T) {
			s := newFrameScanner()
			_, _ = s.Write([]byte("{\"event\":\"run\"}\r\n"))
			if got := <-s.lines; got != `{"event":"run"}` {
				t.Fatalf("line = %q, want CR trimmed", got)
			}
		})

		t.Run("overflow drops rather than blocking the pipe", func(t *testing.T) {
			s := newFrameScanner()
			for i := 0; i < frameLinesCap+10; i++ {
				_, _ = s.Write([]byte("x\n"))
			}
			if s.dropped.Load() != 10 {
				t.Fatalf("dropped = %d, want 10", s.dropped.Load())
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
		_, _ = s.Write([]byte("between-turns"))
		if got := buf.String(); got != "kept" {
			t.Fatalf("sink captured %q, want only the current turn's output", got)
		}
		if got := s.Tail(); !strings.Contains(got, "dropped") || !strings.Contains(got, "between-turns") {
			t.Fatalf("stderr tail = %q, want every write retained for death detail", got)
		}
	})
}

// awaitOutput polls until the buffer carries want: stderr rides its own pipe
// pump, so a terminal frame on stdout can outrun the log write it followed.
func awaitOutput(t *testing.T, buf *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output never carried %q: %q", want, buf.String())
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

// turnScript is a protocol-speaking resident: per turn it reads frames until the
// run frame, answers one declared-write row keyed by the turn, logs to stderr,
// and echoes done with the turn number it saw in the go frame.
const turnScript = `while read line; do
  case "$line" in
  *'"go"'*)
    turn=$(printf '%s' "$line" | sed 's/.*"turn"://;s/[^0-9].*//')
    ;;
  *'"run"'*)
    echo "turn $turn ran" >&2
    printf '{"event":"row","table":"marts.daily","row":{"day":"d-%s","sum":1}}\n' "$turn"
    printf '{"event":"done","turn":%s}\n' "$turn"
    ;;
  esac
done
`

// testTurnWrites is the declared-writes surface the real-process turns run against.
func testTurnWrites() dispatch.WriteSet {
	return dispatch.WriteSet{"marts.daily": {"day": true, "sum": true}}
}

func TestResidentSessionRealProcess(t *testing.T) {
	t.Run("resident-session-real-process", func(t *testing.T) {
		ctx := context.Background()
		runner := exec.NewOSRunner()

		t.Run("one process answers consecutive turns in place", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, turnScript)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			pgid := ses.handle.PGID()

			first := &lockedBuffer{}
			ses.out.Set(first)
			res := driveTurn(ctx, ses, ses.nextTurn(), nil, testTurnWrites())
			if res.kind != turnDone || len(res.rows) != 1 || res.rows[0].Table != "marts.daily" {
				t.Fatalf("turn 1 = %+v, want done with one declared-write row", res)
			}
			awaitOutput(t, first, "turn 1 ran")
			ses.out.Set(nil)

			second := &lockedBuffer{}
			ses.out.Set(second)
			res = driveTurn(ctx, ses, ses.nextTurn(), nil, testTurnWrites())
			if res.kind != turnDone || len(res.rows) != 1 || string(res.rows[0].Row) != `{"day":"d-2","sum":1}` {
				t.Fatalf("turn 2 = %+v, want done echoing turn 2's row", res)
			}
			awaitOutput(t, second, "turn 2 ran")

			if ses.dead() {
				t.Fatal("session died between turns; the process must stay resident")
			}
			if got := ses.handle.PGID(); got != pgid {
				t.Fatalf("pgid changed across turns: %d -> %d", pgid, got)
			}
			if got := first.String(); !strings.Contains(got, "turn 1 ran") || strings.Contains(got, "turn 2 ran") {
				t.Fatalf("first turn stderr = %q, want only its own output", got)
			}
			if got := second.String(); !strings.Contains(got, "turn 2 ran") || strings.Contains(got, "turn 1 ran") {
				t.Fatalf("second turn stderr = %q, want only its own output", got)
			}

			ses.end()
			if !ses.dead() {
				t.Fatal("end() must reap the process")
			}
		})

		t.Run("input rows are fed and echoed back through the declared writes", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, `while read line; do
  case "$line" in
  *'"go"'*) turn=$(printf '%s' "$line" | sed 's/.*"turn"://;s/[^0-9].*//'); n=0 ;;
  *'"row"'*) n=$((n+1)) ;;
  *'"run"'*)
    printf '{"event":"row","table":"marts.daily","row":{"day":"count","sum":%s}}\n' "$n"
    printf '{"event":"done","turn":%s}\n' "$turn"
    ;;
  esac
done
`)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			defer ses.end()
			feed := []pg.FeedRow{
				{Table: "raw.orders", Row: []byte(`{"id":1}`)},
				{Table: "raw.orders", Row: []byte(`{"id":2}`)},
			}
			res := driveTurn(ctx, ses, ses.nextTurn(), feed, testTurnWrites())
			if res.kind != turnDone || len(res.rows) != 1 || string(res.rows[0].Row) != `{"day":"count","sum":2}` {
				t.Fatalf("fed turn = %+v, want the pipeline to have seen both input rows", res)
			}
		})

		t.Run("pipeline-declared error terminal reports errored", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, `while read line; do
  case "$line" in
  *'"run"'*) printf '{"event":"error","turn":1,"reason":"upstream gone","detail":{"code":3}}\n' ;;
  esac
done
`)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			defer ses.end()
			res := driveTurn(ctx, ses, ses.nextTurn(), nil, testTurnWrites())
			if res.kind != turnErrored || res.end.Reason != "upstream gone" {
				t.Fatalf("errored turn = %+v", res)
			}
			if ses.dead() {
				t.Fatal("a declared error must not end the resident process")
			}
		})

		t.Run("non-frame stdout is a violation quoting the line", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, `while read line; do
  case "$line" in
  *'"run"'*) echo "done 0" ;;
  esac
done
`)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			defer ses.end()
			res := driveTurn(ctx, ses, ses.nextTurn(), nil, testTurnWrites())
			if res.kind != turnViolated || !strings.Contains(res.violation.Error(), `"done 0"`) {
				t.Fatalf("violation = %+v, want the legacy line quoted", res)
			}
		})

		t.Run("one-shot answer then exit is a completed turn, not a death", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, `while read line; do
  case "$line" in
  *'"run"'*)
    printf '{"event":"row","table":"marts.daily","row":{"day":"x","sum":1}}\n'
    printf '{"event":"done","turn":1}\n'
    exit 0
    ;;
  esac
done
`)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			res := driveTurn(ctx, ses, ses.nextTurn(), nil, testTurnWrites())
			if res.kind != turnDone || len(res.rows) != 1 {
				t.Fatalf("one-shot turn = %+v, want done with its row", res)
			}
			select {
			case <-ses.exited:
			case <-time.After(10 * time.Second):
				t.Fatal("one-shot process did not exit")
			}
		})

		t.Run("death mid-turn reports died with the exit status", func(t *testing.T) {
			dir := t.TempDir()
			argv := writeScript(t, dir, `read line
echo "boom" >&2
exit 3
`)
			ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
			if err != nil {
				t.Fatalf("spawnResident: %v", err)
			}
			res := driveTurn(ctx, ses, ses.nextTurn(), nil, testTurnWrites())
			if res.kind != turnDied || res.status.Code != 3 {
				t.Fatalf("death = %+v, want died with exit 3", res)
			}
			if got := ses.out.Tail(); !strings.Contains(got, "boom") {
				t.Fatalf("stderr tail = %q, want the death chatter retained", got)
			}
		})
	})
}
