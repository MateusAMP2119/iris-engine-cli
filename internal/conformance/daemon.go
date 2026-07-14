package conformance

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// socketDialTimeout bounds how long StartDaemon and WaitForSocket poll for a
// daemon's unix socket to accept a connection before giving up.
const socketDialTimeout = 10 * time.Second

// socketPollBackoff caps the backoff between connection attempts while waiting
// for a socket. Readiness is decided solely by a successful connection, never by
// elapsed time: the backoff only keeps the poll from spinning, so this is not a
// fixed sleep standing in for readiness (the no-fixed-sleeps doctrine).
const socketPollBackoff = 200 * time.Millisecond

// stopGrace is how long stop waits for a signalled daemon to exit on its own
// before escalating to a kill. It bounds shutdown, not readiness.
const stopGrace = 5 * time.Second

// WaitForSocket polls until a unix socket at path accepts a connection or ctx is
// done, returning nil on the first successful dial. It waits on connection state
// only; the brief backoff between attempts keeps the loop from spinning and is
// never a substitute for a readiness signal.
func WaitForSocket(ctx context.Context, path string) error {
	var dialer net.Dialer
	backoff := 5 * time.Millisecond
	for {
		conn, err := dialer.DialContext(ctx, "unix", path)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("conformance: socket %s never became ready: %w", path, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff < socketPollBackoff {
			backoff *= 2
		}
	}
}

// HTTPOverSocket returns an *http.Client whose transport dials the given unix
// socket for every request, so daemon HTTP endpoints are reachable over the
// socket exactly as the CLI reaches them. The host in request URLs is ignored;
// use any placeholder host, e.g. http://iris/healthz.
func HTTPOverSocket(path string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", path)
			},
		},
	}
}

// DaemonOptions configures a daemon launch.
type DaemonOptions struct {
	// SocketPath is the unix socket the daemon should listen on; a path under
	// the test's temp directory is used when empty.
	SocketPath string
	// Args are extra arguments appended after the daemon subcommand.
	Args []string
	// Env is extra environment appended to the parent process's environment.
	Env []string
	// ReadyWait bounds the wait for the socket to accept; socketDialTimeout is
	// used when zero.
	ReadyWait time.Duration
}

// Daemon is a running iris daemon process reachable over a unix socket, created
// by Binary.StartDaemon and torn down when the test ends.
//
// The daemon and its managed Postgres are E02's deliverable and do not exist
// yet. StartDaemon builds the process-supervision and socket-readiness
// machinery now, but until the binary grows a real daemon subcommand the
// launched process exits immediately and StartDaemon fails with a clear,
// actionable error rather than hanging. The API is fixed here so E02's
// conformance tests reuse it unchanged.
type Daemon struct {
	// SocketPath is the unix socket the daemon listens on.
	SocketPath string

	cmd     *exec.Cmd
	stderr  *bytes.Buffer
	client  *http.Client
	waited  chan struct{}
	waitErr error
}

// StartDaemon launches the binary as a foreground daemon and waits for its unix
// socket to accept a connection before returning, registering cleanup that
// signals the process and waits for it to exit. It fails the test with a clear
// error if the process dies before its socket is ready -- the expected outcome
// until E02 gives the binary a real daemon subcommand and managed Postgres.
func (b *Binary) StartDaemon(t testing.TB, opts DaemonOptions) *Daemon {
	t.Helper()

	socket := opts.SocketPath
	if socket == "" {
		socket = filepath.Join(t.TempDir(), "iris.sock")
	}
	ready := opts.ReadyWait
	if ready == 0 {
		ready = socketDialTimeout
	}

	args := []string{"daemon", "--socket", socket}
	args = append(args, opts.Args...)
	cmd := exec.Command(b.path, args...)
	env := cmd.Environ()
	env = append(env, opts.Env...)
	cmd.Env = env

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	d := &Daemon{
		SocketPath: socket,
		cmd:        cmd,
		stderr:     &stderr,
		client:     HTTPOverSocket(socket),
		waited:     make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("conformance: starting daemon process: %v", err)
	}
	go func() {
		d.waitErr = cmd.Wait()
		close(d.waited)
	}()
	t.Cleanup(d.stop)

	ctx, cancel := context.WithTimeout(context.Background(), ready)
	defer cancel()
	readyCh := make(chan error, 1)
	go func() { readyCh <- WaitForSocket(ctx, socket) }()

	select {
	case <-d.waited:
		t.Fatalf("conformance: daemon exited before its socket %s was ready (%v). "+
			"The daemon subcommand and its managed Postgres are E02's deliverable; "+
			"this harness is ready to drive them.\nstderr:\n%s", socket, d.waitErr, stderr.String())
	case err := <-readyCh:
		if err != nil {
			t.Fatalf("conformance: daemon socket %s not ready: %v\nstderr:\n%s", socket, err, stderr.String())
		}
	}
	return d
}

// Client is the HTTP client bound to the daemon's socket, for callers needing
// full control over the request they issue.
func (d *Daemon) Client() *http.Client { return d.client }

// Get issues an HTTP GET to path (e.g. "/healthz") over the daemon's socket and
// returns the response, failing the test if the request cannot be built or sent.
// The caller closes the response body.
func (d *Daemon) Get(t testing.TB, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://iris"+path, nil)
	if err != nil {
		t.Fatalf("conformance: building GET %s: %v", path, err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		t.Fatalf("conformance: GET %s over %s: %v", path, d.SocketPath, err)
	}
	return resp
}

// stop signals the daemon and waits for it to exit, escalating to a kill after a
// grace period. It is idempotent and safe to call when the process has already
// exited.
func (d *Daemon) stop() {
	if d.cmd.Process == nil {
		return
	}
	select {
	case <-d.waited:
		return
	default:
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.waited:
	case <-time.After(stopGrace):
		_ = d.cmd.Process.Kill()
		<-d.waited
	}
}
