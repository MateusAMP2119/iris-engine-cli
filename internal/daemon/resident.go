package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
)

// This file is the resident-pipeline session machinery under the turn protocol
// (#206, extending #192): a pipeline process stays alive across turns, iterating
// on the JSON Lines protocol -- the engine writes go/row/run frames to its stdin,
// the process answers row frames and one done/error terminal on stdout -- so
// spawn costs are paid once, not per turn. stdout is protocol-only (every line is
// a frame; a non-frame line is a protocol violation the turn driver dead-letters
// with the line quoted); stderr stays free-form log, captured per turn and tailed
// for process-death detail. The session holds the pipes and the exit state; the
// pure frame semantics live in dispatch's turn model.

// scanBufferCap bounds the frame scanner's partial-line buffer; a longer
// unterminated line is delivered as one oversized line (it fails frame parse, so
// the protocol's violation path bounds it).
const scanBufferCap = 1 << 20

// frameLinesCap bounds the scanner's undelivered-lines channel. A turn's frames
// are consumed live by the turn driver; only stray output between turns can back
// up, and past the cap it is dropped (counted) rather than blocking the pipe.
const frameLinesCap = 4096

// stderrTailCap bounds the retained stderr tail used as process-death detail.
const stderrTailCap = 2048

// tailRing retains the last stderrTailCap bytes written through it.
type tailRing struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends p, keeping only the tail.
func (r *tailRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - stderrTailCap; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	return len(p), nil
}

// String returns the retained tail.
func (r *tailRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// switchSink is a concurrency-safe output writer whose destination swaps per
// turn; a nil destination discards. Every write also feeds the tail ring, so a
// process death between turns still carries recent stderr in its detail.
type switchSink struct {
	tail tailRing
	mu   sync.Mutex
	w    io.Writer
}

// Set points the sink at the current turn's log buffer (nil between turns).
func (s *switchSink) Set(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

// Write forwards to the current destination under the lock (a Set waits out an
// in-flight write), best-effort: capture never fails the process's output pipe.
func (s *switchSink) Write(p []byte) (int, error) {
	_, _ = s.tail.Write(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		_, _ = s.w.Write(p)
	}
	return len(p), nil
}

// Tail returns the retained stderr tail.
func (s *switchSink) Tail() string { return s.tail.String() }

// frameScanner splits a resident process's stdout into protocol lines and
// delivers each on the lines channel. stdout is protocol-only under the turn
// protocol, so there is no log routing here; a line arriving while no turn is
// consuming backs up to frameLinesCap and is then dropped (counted), never
// blocking the process's pipe.
type frameScanner struct {
	mu      sync.Mutex
	buf     []byte
	lines   chan string
	dropped atomic.Int64
}

// newFrameScanner builds the stdout frame scanner.
func newFrameScanner() *frameScanner {
	return &frameScanner{lines: make(chan string, frameLinesCap)}
}

// Write buffers to line boundaries and delivers whole lines; it never errors
// (capture is best-effort) and never blocks on a full channel.
func (p *frameScanner) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		p.deliver(string(bytes.TrimSuffix(p.buf[:i], []byte("\r"))))
		p.buf = p.buf[i+1:]
	}
	if len(p.buf) > scanBufferCap {
		p.deliver(string(p.buf))
		p.buf = nil
	}
	return len(b), nil
}

// deliver hands one line to the channel, dropping (counted) when full.
func (p *frameScanner) deliver(line string) {
	select {
	case p.lines <- line:
	default:
		p.dropped.Add(1)
	}
}

// residentSession is one live pipeline process iterating in place: its handle,
// protocol pipes, per-session turn counter, and terminal state once it exits.
type residentSession struct {
	key       string
	handle    exec.Handle
	stdin     *os.File
	scanner   *frameScanner
	out       *switchSink
	exited    chan struct{}
	status    exec.ExitStatus
	waitErr   error
	turn      int64
	cancelled atomic.Bool
}

// spawnResident starts a pipeline process wired for the turn protocol: stdin over
// an OS pipe, stdout through the frame scanner, stderr to the switchable sink.
// The child environment carries no database credentials -- the engine mediates
// every database access (#206).
func spawnResident(ctx context.Context, runner exec.Runner, key, dir string, argv, env []string) (*residentSession, error) {
	out := &switchSink{}
	scanner := newFrameScanner()
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
		_ = pw.Close() // unblock any frame writer once the process is gone
		close(s.exited)
	}()
	return s, nil
}

// nextTurn advances and returns the session's turn number. Turn numbers are
// per-session and in-memory: a respawned session starts over, and the terminal
// echo check is always against the turn just issued.
func (s *residentSession) nextTurn() int64 {
	s.turn++
	return s.turn
}

// send writes one engine frame line; an error means the process is gone (its
// exit reports through exited).
func (s *residentSession) send(line string) error {
	_, err := s.stdin.WriteString(line + "\n")
	return err
}

// drainFrames discards stale stdout lines a prior turn left behind, so a new
// turn's collector never reads a dead turn's frames (stale echoes after a cancel
// are detected and dropped here).
func (s *residentSession) drainFrames() {
	for {
		select {
		case <-s.scanner.lines:
		default:
			return
		}
	}
}

// markCancelled records an operator cancel, so the turn driver records the
// process death as the cancel's park rather than minting a failed dead letter.
func (s *residentSession) markCancelled() { s.cancelled.Store(true) }

// wasCancelled reports whether an operator cancel ended this session.
func (s *residentSession) wasCancelled() bool { return s.cancelled.Load() }

// dead reports whether the process already exited.
func (s *residentSession) dead() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// end stops the session: stdin EOF (the polite signal), then a group kill, then
// the reap.
func (s *residentSession) end() {
	_ = s.stdin.Close()
	_ = s.handle.Kill()
	<-s.exited
}

// residentRuns is the registry of live resident sessions, one per pipeline. It is
// daemon-scoped so the cancel plane can reach a session the lane loop owns; the
// sessions themselves die with their leadership term (the runner kills the group
// on term-context cancellation) and the lane loop replaces dead entries.
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

// cancel marks and ends the pipeline's live session, reporting whether one was
// there: the operator-stop path kills the worker after parking the pipeline, and
// the mark keeps the turn driver from minting a failed dead letter for the kill.
func (r *residentRuns) cancel(pipeline string) bool {
	s := r.get(pipeline)
	if s == nil {
		return false
	}
	s.markCancelled()
	s.end()
	return true
}
