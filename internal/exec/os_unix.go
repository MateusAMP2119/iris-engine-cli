//go:build unix

package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"sync"
	"syscall"
	"time"
)

// OSRunner is the real subprocess runner: it spawns an OS process in its own
// process group, captures its output, and kills the whole group on Kill or
// context cancellation. It is the seed of E05.1's exec seam. Unix only
// (darwin + linux); Windows is deferred from v1 (section 16).
type OSRunner struct{}

// NewOSRunner returns the real subprocess runner.
func NewOSRunner() *OSRunner { return &OSRunner{} }

// compile-time proof the real runner satisfies the seam.
var _ Runner = (*OSRunner)(nil)

const (
	// drainWindow is how long, after the subprocess is reaped, a pump waits for
	// the next byte before concluding the output has fully drained. Each read that
	// makes progress buys another window, so the child's own buffered output is
	// captured no matter how slow the destination writer is; the pump stops after
	// a window elapses with no progress. Wait keys off the child's reap, so a run
	// with no lingering descendant reaches pipe EOF at child exit and never waits
	// on this at all.
	drainWindow = 100 * time.Millisecond
	// drainBudget caps total post-reap draining, so a descendant that keeps the
	// pipe perpetually busy cannot stall Wait forever; output past the budget may
	// be truncated.
	drainBudget = 2 * time.Second
)

// Start spawns spec as a direct exec (never a shell) in its own process group,
// streaming stdout and stderr to the spec's writers. The returned Handle's PGID
// is the new group's id. When ctx is cancelled the group is killed.
//
// Wait returns as soon as the subprocess itself is reaped, not when descendants
// that inherited its output pipe exit, so a pipeline that backgrounds a
// long-lived grandchild never stalls the run. Output the subprocess produced
// before it exited is captured; output a surviving descendant writes after the
// reap may be truncated. When Stdout and Stderr are the same comparable writer,
// both streams are captured through one pipe and one pump, so they are never
// written concurrently.
func (r *OSRunner) Start(ctx context.Context, spec Spec) (Handle, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("exec: empty argv")
	}
	cmd := osexec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	// Setpgid puts the child in a new process group whose id equals its pid, so
	// killing the negative pgid reaches the child and every descendant.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	streams, err := wireOutput(cmd, spec)
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		for _, cs := range streams {
			cs.closeEnds()
		}
		return nil, fmt.Errorf("exec: start %v: %w", spec.Argv, err)
	}

	h := &osHandle{cmd: cmd, pgid: cmd.Process.Pid, streams: streams, done: make(chan struct{})}

	// The parent drops its copies of the write ends so a read end reaches EOF once
	// the child and every descendant have closed theirs. os/exec hands an *os.File
	// straight to the child without tracking it, so this close is ours to make.
	for _, cs := range streams {
		_ = cs.w.Close()
	}

	// Drain each pipe on its own goroutine that survives Wait, keyed off the
	// child's reap rather than pipe EOF.
	for _, cs := range streams {
		cs := cs
		h.drain.Add(1)
		go func() {
			defer h.drain.Done()
			h.pump(cs)
		}()
	}

	// Reap the child on a Start-owned goroutine: recording its status and closing
	// done at reap time disarms the cancel watcher exactly when the pgid becomes
	// eligible for recycling, so a late cancel can no longer signal a recycled
	// group.
	go h.reap()

	// Cancelling ctx kills the group; the watcher exits at reap (done), so it
	// never outlives the process it guards and never leaks.
	go func() {
		select {
		case <-ctx.Done():
			_ = h.Kill()
		case <-h.done:
		}
	}()

	return h, nil
}

// wireOutput wires the child's stdout and stderr to spec's writers and returns
// the pipes we must pump. A writer already backed by an *os.File (or a nil
// writer) is handed to the child directly, so os/exec dup2s it with no copy
// goroutine cmd.Wait would block on. A plain writer is fed by a pipe we own end
// to end. When Stdout and Stderr are the same comparable writer, both child fds
// are fed from one pipe drained by a single pump -- mirroring os/exec's own
// same-writer dedup -- so the two never call Write concurrently.
func wireOutput(cmd *osexec.Cmd, spec Spec) ([]*copyStream, error) {
	if spec.Stdout != nil && interfaceEqual(spec.Stdout, spec.Stderr) {
		child, cs, err := childOutput(spec.Stdout)
		if err != nil {
			return nil, err
		}
		if child != nil {
			// The same *os.File in both slots: os/exec's interfaceEqual feeds the
			// child's stdout and stderr from the one descriptor.
			cmd.Stdout = child
			cmd.Stderr = child
		}
		return streamList(cs), nil
	}

	outChild, outStream, err := childOutput(spec.Stdout)
	if err != nil {
		return nil, err
	}
	errChild, errStream, err := childOutput(spec.Stderr)
	if err != nil {
		if outStream != nil {
			outStream.closeEnds()
		}
		return nil, err
	}
	if outChild != nil {
		cmd.Stdout = outChild
	}
	if errChild != nil {
		cmd.Stderr = errChild
	}
	return streamList(outStream, errStream), nil
}

// streamList collects the non-nil copy streams.
func streamList(streams ...*copyStream) []*copyStream {
	var out []*copyStream
	for _, cs := range streams {
		if cs != nil {
			out = append(out, cs)
		}
	}
	return out
}

// interfaceEqual reports whether a and b are the same writer, mirroring os/exec's
// own comparison: it recovers from the panic == raises on an uncomparable dynamic
// type and treats that as not-equal.
func interfaceEqual(a, b io.Writer) (eq bool) {
	defer func() {
		if recover() != nil {
			eq = false
		}
	}()
	return a == b
}

// copyStream is one wrapped output stream: the caller's writer, the pipe read
// end we drain into it, and the write end the parent closes after Start.
type copyStream struct {
	dst io.Writer
	r   *os.File
	w   *os.File
	err error // writer error, read only after the drain goroutine has finished
}

// closeEnds releases both pipe ends; used only on a Start failure, before the
// drain goroutine takes ownership of the read end.
func (cs *copyStream) closeEnds() {
	_ = cs.r.Close()
	_ = cs.w.Close()
}

// childOutput prepares one output stream for the child. It returns the *os.File
// the child should write to and, when dst is a plain writer we must pump
// ourselves, the pipe wrapping it. A nil dst discards the stream (nil child
// file); an *os.File dst is handed to the child directly.
func childOutput(dst io.Writer) (*os.File, *copyStream, error) {
	if dst == nil {
		return nil, nil, nil
	}
	if f, ok := dst.(*os.File); ok {
		return f, nil, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("exec: pipe: %w", err)
	}
	return pw, &copyStream{dst: dst, r: pr, w: pw}, nil
}

// osHandle is a started OS subprocess and its process group.
type osHandle struct {
	cmd            *osexec.Cmd
	pgid           int
	streams        []*copyStream
	drain          sync.WaitGroup // the output pump goroutines
	done           chan struct{}  // closed when the child is reaped
	budgetDeadline time.Time      // hard cap on post-reap draining; set before done closes

	mu      sync.Mutex // guards reaped, status, waitErr
	reaped  bool
	status  ExitStatus
	waitErr error
}

// pump drains cs.r into cs.dst. Before the child is reaped it streams live with
// no deadline. Once reaped it drains on a progress-extended deadline: each read
// that makes progress buys another drainWindow, so the child's buffered output
// is captured however slow dst is; it stops after a window with no progress, or
// when the total post-reap budget is spent. A write failure is recorded (and
// surfaced from Wait); a read-side stop -- EOF, the no-progress deadline, or a
// closed read end -- is not an error.
func (h *osHandle) pump(cs *copyStream) {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := cs.r.Read(buf)
		if n > 0 {
			if _, werr := cs.dst.Write(buf[:n]); werr != nil {
				cs.err = werr
				return
			}
		}
		if rerr != nil {
			// Deadlines are armed only after reap. A non-deadline error (EOF or a
			// closed read end), or a deadline that yielded no bytes (a full window
			// with no progress), ends the drain; a deadline that still delivered
			// bytes is progress, so keep going.
			if !errors.Is(rerr, os.ErrDeadlineExceeded) || n == 0 {
				return
			}
		}
		// After reap, roll the read deadline forward on progress, capped by the
		// total budget so a perpetually busy descendant cannot stall Wait.
		select {
		case <-h.done:
			if !time.Now().Before(h.budgetDeadline) {
				return
			}
			next := time.Now().Add(drainWindow)
			if next.After(h.budgetDeadline) {
				next = h.budgetDeadline
			}
			_ = cs.r.SetReadDeadline(next)
		default:
		}
	}
}

// reap waits on the child, records its terminal status, disarms the cancel
// watcher, and kicks the pumps into their bounded post-reap drain.
func (h *osHandle) reap() {
	st, werr := translateWait(h.cmd.Wait(), h.cmd)

	h.mu.Lock()
	h.reaped = true
	h.status = st
	h.waitErr = werr
	h.mu.Unlock()

	h.budgetDeadline = time.Now().Add(drainBudget)
	close(h.done)

	// Kick each pump out of its unbounded live read into the progress-extended
	// post-reap drain; it manages its own deadline from here.
	initial := time.Now().Add(drainWindow)
	for _, cs := range h.streams {
		_ = cs.r.SetReadDeadline(initial)
	}
}

// PGID returns the subprocess's process-group id.
func (h *osHandle) PGID() int { return h.pgid }

// Wait waits for the subprocess to terminate and returns its exit status,
// translating a signaled termination into a terminal status rather than an
// error. It returns when the subprocess itself is reaped -- not when a
// backgrounded descendant that inherited the output pipe finally exits -- so a
// daemonizing pipeline never blocks it. After Wait returns, the captured output
// is complete and safe to read.
func (h *osHandle) Wait() (ExitStatus, error) {
	<-h.done       // the child has been reaped; its status is recorded
	h.drain.Wait() // its output has drained (to EOF, or to the post-reap grace)

	h.mu.Lock()
	st, werr := h.status, h.waitErr
	h.mu.Unlock()
	if werr != nil {
		return st, werr
	}
	for _, cs := range h.streams {
		if cs.err != nil {
			return st, cs.err
		}
	}
	return st, nil
}

// Kill terminates the whole process group with SIGKILL. Killing the negated pgid
// signals every member of the group. Once the subprocess has been reaped its
// pgid may be recycled, so Kill is then a documented no-op rather than a signal
// that could reach an unrelated group.
func (h *osHandle) Kill() error {
	h.mu.Lock()
	reaped := h.reaped
	h.mu.Unlock()
	if reaped {
		return nil
	}
	if err := syscall.Kill(-h.pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // group already gone
		}
		return fmt.Errorf("exec: kill group %d: %w", h.pgid, err)
	}
	return nil
}

// translateWait maps an os/exec Wait error into an ExitStatus, reporting a
// signaled termination as a terminal status rather than an error.
func translateWait(err error, cmd *osexec.Cmd) (ExitStatus, error) {
	if err == nil {
		return ExitStatus{Code: 0}, nil
	}
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return ExitStatus{Code: -1, Signaled: true, Signal: ws.Signal()}, nil
			}
			return ExitStatus{Code: ws.ExitStatus()}, nil
		}
		return ExitStatus{Code: ee.ExitCode()}, nil
	}
	return ExitStatus{}, fmt.Errorf("exec: wait %v: %w", cmd.Args, err)
}
