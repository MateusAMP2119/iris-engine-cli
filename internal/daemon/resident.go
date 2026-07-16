package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
)

// This file is the resident-pipeline machinery (#192): a pipeline process may stay alive across runs, iterating on a line protocol -- the engine writes "go <run_id>" to its stdin, the script answers "done <status>" on stdout -- so spawn and connect costs are paid once, not per iteration. A process that simply exits is a legacy one-shot run, byte-for-byte today's behavior.

// residentDoneWord is the stdout protocol verb a resident script answers an iteration with ("done <int>").
const residentDoneWord = "done"

// residentGoWord is the stdin protocol verb the engine starts an iteration with ("go <run_id>").
const residentGoWord = "go"

// scanBufferCap bounds the protocol scanner's partial-line buffer; a longer unterminated line is flushed to the log as-is.
const scanBufferCap = 1 << 20

// switchSink is a concurrency-safe output writer whose destination swaps per iteration; a nil destination discards.
type switchSink struct {
	mu sync.Mutex
	w  io.Writer
}

// Set points the sink at the current iteration's log (nil between iterations).
func (s *switchSink) Set(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

// Write forwards to the current destination under the lock (a Set waits out an in-flight write), best-effort: capture never fails the process's output pipe.
func (s *switchSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		_, _ = s.w.Write(p)
	}
	return len(p), nil
}

// doneEvent is one parsed "done <status>" protocol line.
type doneEvent struct {
	code int
}

// protocolScanner splits a resident process's stdout into lines, consuming "done <int>" protocol lines onto done and forwarding everything else to sink.
type protocolScanner struct {
	mu   sync.Mutex
	buf  []byte
	sink io.Writer
	done chan doneEvent
}

// newProtocolScanner builds the stdout scanner over the per-iteration sink.
func newProtocolScanner(sink io.Writer) *protocolScanner {
	return &protocolScanner{sink: sink, done: make(chan doneEvent, 1)}
}

// Write buffers to line boundaries, routing protocol lines to done and log lines to the sink; it never errors (capture is best-effort).
func (p *protocolScanner) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := p.buf[:i+1]
		if code, ok := parseDoneLine(string(line[:i])); ok {
			select {
			case p.done <- doneEvent{code: code}: // a second done for one go is a protocol violation; the extra is dropped
			default:
			}
		} else {
			_, _ = p.sink.Write(line)
		}
		p.buf = p.buf[i+1:]
	}
	if len(p.buf) > scanBufferCap {
		_, _ = p.sink.Write(p.buf)
		p.buf = nil
	}
	return len(b), nil
}

// parseDoneLine reports whether a stdout line is a protocol "done <int>" answer and its status code.
func parseDoneLine(line string) (int, bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != residentDoneWord {
		return 0, false
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// residentSession is one live pipeline process iterating in place: its handle, protocol pipes, and terminal state once it exits.
type residentSession struct {
	key     string
	handle  exec.Handle
	stdin   *os.File
	scanner *protocolScanner
	out     *switchSink
	exited  chan struct{}
	status  exec.ExitStatus
	waitErr error
}

// spawnResident starts a pipeline process wired for the iteration protocol: stdin over an OS pipe, stdout through the protocol scanner, stderr to the switchable sink.
func spawnResident(ctx context.Context, runner exec.Runner, key, dir string, argv, env []string) (*residentSession, error) {
	out := &switchSink{}
	scanner := newProtocolScanner(out)
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("resident stdin pipe: %w", err)
	}
	h, err := runner.Start(ctx, exec.Spec{Dir: dir, Argv: argv, Env: env, Stdout: scanner, Stderr: out, Stdin: pr})
	_ = pr.Close() // the child holds its own read end; the parent's copy must not keep the pipe alive past the child
	if err != nil {
		_ = pw.Close()
		return nil, err
	}
	s := &residentSession{key: key, handle: h, stdin: pw, scanner: scanner, out: out, exited: make(chan struct{})}
	go func() {
		s.status, s.waitErr = h.Wait()
		_ = pw.Close() // unblock any go-line writer once the process is gone
		close(s.exited)
	}()
	return s, nil
}

// sendGo writes the iteration-start line; an error means the process is gone (its exit reports through exited).
func (s *residentSession) sendGo(runID int64) error {
	_, err := s.stdin.WriteString(residentGoWord + " " + strconv.FormatInt(runID, 10) + "\n")
	return err
}

// drainDone discards a stale done event a prior iteration left behind.
func (s *residentSession) drainDone() {
	select {
	case <-s.scanner.done:
	default:
	}
}

// dead reports whether the process already exited.
func (s *residentSession) dead() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// end stops the session: stdin EOF (the polite signal), then a group kill, then the reap.
func (s *residentSession) end() {
	_ = s.stdin.Close()
	_ = s.handle.Kill()
	<-s.exited
}

// residentRuns is the per-leadership-term registry of live resident sessions, one per pipeline.
type residentRuns struct {
	mu sync.Mutex
	m  map[string]*residentSession
}

// newResidentRuns builds an empty registry.
func newResidentRuns() *residentRuns {
	return &residentRuns{m: map[string]*residentSession{}}
}

// get returns the pipeline's live session, if any.
func (r *residentRuns) get(pipeline string) *residentSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[pipeline]
}

// put records the pipeline's live session.
func (r *residentRuns) put(pipeline string, s *residentSession) {
	r.mu.Lock()
	r.m[pipeline] = s
	r.mu.Unlock()
}

// drop forgets the pipeline's session (already ended or exited).
func (r *residentRuns) drop(pipeline string) {
	r.mu.Lock()
	delete(r.m, pipeline)
	r.mu.Unlock()
}
