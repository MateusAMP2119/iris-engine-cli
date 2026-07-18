package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/plugin"
)

// This file is #215 stage 3's service supervision: long-running service-plugin
// instances the daemon spawns and the engine speaks to over the instance's own
// stdio. The service wire contract mirrors the run-side call frames, one JSON
// line each way: the engine writes {"call":N,"verb":"fetch","args":{...}} to
// the instance's stdin and reads {"call":N,"ok":{...}} or {"call":N,"err":"..."}
// from its stdout; stderr is free-form, retained as a tail for death detail.
// The script never sees the instance -- verbs cross the run boundary as frames,
// and the daemon relays them here. One call is serviced at a time per instance
// (the instance lock), so a shared lane or resident instance serializes its
// callers.
//
// Lifetimes: a run instance is spawned for one turn and ended with it; a lane
// instance is shared by the lane's members and keyed by the lane; a resident
// instance is shared engine-wide, keyed by name@version, and survives across
// runs until the daemon exits (its state visible in lineage through the
// instance id every attaching run records). fresh: true replaces the shared
// instance with a cold one before the run attaches.

// serviceLineCap bounds one service reply line: large enough to carry a
// spillable payload across the pipe, small enough to bound a runaway instance.
const serviceLineCap = 64 << 20

// serviceRequest is one engine→service call line.
type serviceRequest struct {
	Call int64           `json:"call"`
	Verb string          `json:"verb"`
	Args json.RawMessage `json:"args,omitempty"`
}

// serviceResponse is one service→engine reply line.
type serviceResponse struct {
	Call int64           `json:"call"`
	OK   json.RawMessage `json:"ok,omitempty"`
	Err  string          `json:"err,omitempty"`
}

// serviceSession is one live service-plugin instance: its process handle,
// protocol pipes, and the identity every attaching run records.
type serviceSession struct {
	// id is the ledger-visible instance identity: "<name>@<version>#<spawn seq>".
	id      string
	handle  exec.Handle
	stdin   *os.File
	scanner *frameScanner
	out     *switchSink
	exited  chan struct{}
	status  exec.ExitStatus

	mu      sync.Mutex // one call at a time per instance
	callSeq int64
}

// spawnService starts one service instance wired for the line protocol: the
// binary alone as argv (a service owns its whole lifetime; verbs arrive as
// lines), scrubbed environment, no working-directory promises beyond a private
// temp dir the instance may scribble in.
func spawnService(ctx context.Context, runner exec.Runner, id string, res *plugin.Resolved) (*serviceSession, error) {
	dir, err := os.MkdirTemp("", "iris-service-*")
	if err != nil {
		return nil, fmt.Errorf("service %s: workdir: %w", id, err)
	}
	out := &switchSink{}
	scanner := newFrameScannerCap(serviceLineCap)
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("service %s: stdin pipe: %w", id, err)
	}
	h, err := runner.Start(ctx, exec.Spec{
		Dir:    dir,
		Argv:   []string{res.Binary},
		Env:    []string{}, // scrubbed by design: a plugin sees nothing of the daemon's environment
		Stdout: scanner,
		Stderr: out,
		Stdin:  pr,
	})
	_ = pr.Close() // the child holds its own read end
	if err != nil {
		_ = pw.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("service %s: start: %w", id, err)
	}
	s := &serviceSession{id: id, handle: h, stdin: pw, scanner: scanner, out: out, exited: make(chan struct{})}
	go func() {
		s.status, _ = h.Wait()
		_ = pw.Close()
		_ = os.RemoveAll(dir)
		close(s.exited)
	}()
	return s, nil
}

// dead reports whether the instance's process already exited.
func (s *serviceSession) dead() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// end stops the instance: stdin EOF (the polite signal), group kill, reap.
func (s *serviceSession) end() {
	_ = s.stdin.Close()
	_ = s.handle.Kill()
	<-s.exited
}

// call services one verb call on this instance: write the request line, read
// reply lines until the echoed call number arrives (stale replies from an
// abandoned earlier call are dropped), bounded by timeout. A timeout kills the
// instance -- a wedged service's state is not worth carrying -- and the caller
// respawns on the next acquire.
func (s *serviceSession) call(ctx context.Context, verb string, args json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.callSeq++
	req, err := json.Marshal(serviceRequest{Call: s.callSeq, Verb: verb, Args: args})
	if err != nil {
		return nil, fmt.Errorf("service %s: encode request: %w", s.id, err)
	}
	if _, err := s.stdin.WriteString(string(req) + "\n"); err != nil {
		return nil, fmt.Errorf("service %s exited before the call was written", s.id)
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		select {
		case line := <-s.scanner.lines:
			var resp serviceResponse
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				s.end()
				return nil, fmt.Errorf("service %s wrote a non-protocol line %q; instance ended", s.id, line)
			}
			if resp.Call != s.callSeq {
				continue // a stale reply from a call this side stopped waiting on
			}
			if resp.Err != "" {
				return nil, fmt.Errorf("service %s verb %q: %s", s.id, verb, resp.Err)
			}
			if len(resp.OK) == 0 {
				return nil, fmt.Errorf("service %s verb %q replied with neither ok nor err", s.id, verb)
			}
			return resp.OK, nil
		case <-s.exited:
			return nil, fmt.Errorf("service %s exited mid-call (%s)", s.id, exitDetail(s.status))
		case <-cctx.Done():
			s.end() // a wedged instance's state is not worth carrying
			return nil, fmt.Errorf("service %s verb %q timed out after %s; instance ended", s.id, verb, timeout)
		}
	}
}

// pluginServices is the daemon-scoped service-instance registry: lane and
// resident instances live here between turns, keyed by their sharing scope, and
// every spawn draws a monotonic instance sequence so an instance id never
// repeats within a daemon lifetime. Run-lifetime instances are never registered
// -- the turn that spawned them ends them. Instances are spawned under the
// registry's base context, so daemon shutdown kills every group.
type pluginServices struct {
	ctx    context.Context
	runner exec.Runner
	logger *slog.Logger

	mu  sync.Mutex
	seq int64
	m   map[string]*serviceSession
}

// newPluginServices builds the registry; ctx is the daemon lifetime every
// instance is spawned under.
func newPluginServices(ctx context.Context, runner exec.Runner, logger *slog.Logger) *pluginServices {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &pluginServices{ctx: ctx, runner: runner, logger: logger, m: map[string]*serviceSession{}}
}

// nextID mints the next instance id for a resolved plugin.
func (ps *pluginServices) nextID(res *plugin.Resolved) string {
	ps.seq++
	return fmt.Sprintf("%s@%s#%d", res.Manifest.Name, res.Manifest.Version, ps.seq)
}

// spawnRunInstance starts an unregistered run-lifetime instance; the caller
// owns ending it when its turn ends.
func (ps *pluginServices) spawnRunInstance(res *plugin.Resolved) (*serviceSession, error) {
	ps.mu.Lock()
	id := ps.nextID(res)
	ps.mu.Unlock()
	return spawnService(ps.ctx, ps.runner, id, res)
}

// acquire returns the live shared instance for key, spawning one when none is
// live. fresh replaces a live instance with a cold one before returning, so the
// attaching run never sees carried state.
func (ps *pluginServices) acquire(key string, res *plugin.Resolved, fresh bool) (*serviceSession, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	s := ps.m[key]
	if s != nil && (s.dead() || fresh) {
		if !s.dead() {
			ps.logger.Info("plugin service replaced for a fresh binding", "instance", s.id, "key", key)
		}
		s.end()
		delete(ps.m, key)
		s = nil
	}
	if s == nil {
		var err error
		s, err = spawnService(ps.ctx, ps.runner, ps.nextID(res), res)
		if err != nil {
			return nil, err
		}
		ps.m[key] = s
	}
	return s, nil
}

// endAll ends every registered shared instance (daemon shutdown; the spawn
// context's cancellation kills the groups too, this reaps them deterministically).
func (ps *pluginServices) endAll() {
	ps.mu.Lock()
	sessions := make([]*serviceSession, 0, len(ps.m))
	for k, s := range ps.m {
		sessions = append(sessions, s)
		delete(ps.m, k)
	}
	ps.mu.Unlock()
	for _, s := range sessions {
		s.end()
	}
}
