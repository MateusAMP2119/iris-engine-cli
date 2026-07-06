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

// drainGrace bounds how long, after the subprocess is reaped, the output copy
// goroutines keep draining a stdout/stderr pipe a surviving descendant still
// holds open. Wait keys off the child's reap, not pipe EOF, so a daemonizing
// pipeline never stalls it; this grace only lets the child's own already-buffered
// output flush before the read ends are released. It is not on the hot path: a
// run with no lingering descendant reaches pipe EOF at child exit and never waits
// for it.
const drainGrace = 100 * time.Millisecond

// Start spawns spec as a direct exec (never a shell) in its own process group,
// streaming stdout and stderr to the spec's writers. The returned Handle's PGID
// is the new group's id. When ctx is cancelled the group is killed.
//
// Wait returns as soon as the subprocess itself is reaped, not when descendants
// that inherited its output pipe exit, so a pipeline that backgrounds a
// long-lived grandchild never stalls the run. Output the subprocess produced
// before it exited is captured; output a surviving descendant writes after the
// reap may be truncated.
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

	// Wire stdout and stderr. A writer already backed by an *os.File (or a nil
	// writer) is handed straight to the child so os/exec dup2s the descriptor with
	// no copy goroutine cmd.Wait would then block on. Any other writer is fed by a
	// pipe we own end to end, so cmd.Wait keys off the child's reap alone.
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

	if err := cmd.Start(); err != nil {
		if outStream != nil {
			outStream.closeEnds()
		}
		if errStream != nil {
			errStream.closeEnds()
		}
		return nil, fmt.Errorf("exec: start %v: %w", spec.Argv, err)
	}

	h := &osHandle{cmd: cmd, pgid: cmd.Process.Pid, done: make(chan struct{})}

	// The parent drops its copies of the write ends so a read end reaches EOF once
	// the child and every descendant have closed theirs. os/exec hands an *os.File
	// straight to the child without tracking it, so this close is ours to make.
	for _, cs := range []*copyStream{outStream, errStream} {
		if cs == nil {
			continue
		}
		_ = cs.w.Close()
		h.streams = append(h.streams, cs)
	}

	// Drain each wrapped stream on its own goroutine that survives Wait: it keeps
	// pumping the child's output until EOF or, once the child is reaped, until the
	// post-reap grace releases the read end.
	for _, cs := range h.streams {
		cs := cs
		h.drain.Add(1)
		go func() {
			defer h.drain.Done()
			cs.err = pump(cs.dst, cs.r)
			_ = cs.r.Close()
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

// pump copies r into dst until the read end reports EOF, a deadline, or a close.
// A write failure is returned (a capture failure surfaced from Wait); a read-side
// stop -- EOF, the post-reap deadline, or a closed read end -- is not an error.
func pump(dst io.Writer, r *os.File) error {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			return nil
		}
	}
}

// osHandle is a started OS subprocess and its process group.
type osHandle struct {
	cmd     *osexec.Cmd
	pgid    int
	streams []*copyStream
	drain   sync.WaitGroup // the output pump goroutines
	done    chan struct{}  // closed when the child is reaped

	mu      sync.Mutex // guards reaped, status, waitErr
	reaped  bool
	status  ExitStatus
	waitErr error
}

// reap waits on the child, records its terminal status, disarms the cancel
// watcher, and arms the bounded output drain.
func (h *osHandle) reap() {
	st, werr := translateWait(h.cmd.Wait(), h.cmd)

	h.mu.Lock()
	h.reaped = true
	h.status = st
	h.waitErr = werr
	h.mu.Unlock()
	close(h.done)

	// The child is gone, so its own output is now buffered in the pipes. Give the
	// pumps a bounded grace to flush that buffer, then release the read ends so a
	// surviving descendant holding the write end cannot keep them open forever.
	deadline := time.Now().Add(drainGrace)
	for _, cs := range h.streams {
		_ = cs.r.SetReadDeadline(deadline)
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
