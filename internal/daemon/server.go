package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file is the daemon's listener wiring: the one http.Handler (the api mux)
// served on two listeners. The unix socket is always present -- local-only,
// filesystem-permission-guarded, ambient authorization, zero config. The TCP
// listener is opt-in (settings.TCP), PAT-gated on every request, and TLS-wrapped
// when a cert/key pair is configured. Serving HTTP over listeners is the daemon's
// job, so this is proven at the integration tier with real sockets and no database.

const (
	// socketFilePerm is the mode the control socket file is clamped to after bind:
	// owner read/write only. The control plane is local-only, guarded by filesystem
	// permissions.
	socketFilePerm os.FileMode = 0o600
	// serverReadHeaderTimeout bounds request header reads (a Slowloris guard) on
	// both listeners.
	serverReadHeaderTimeout = 10 * time.Second
	// serverShutdownGrace bounds graceful shutdown before listeners are forced
	// closed.
	serverShutdownGrace = 5 * time.Second
)

// Server owns the daemon's listeners: the always-on unix control socket and the
// optional PAT-gated (and optionally TLS) TCP listener, both serving one shared
// http.Handler. Build it with NewServer, bring it up with Start, block a
// foreground daemon on Serve, and tear it down with Shutdown.
type Server struct {
	settings config.Settings
	handler  http.Handler
	verifier api.TokenVerifier
	logger   *slog.Logger

	mu       sync.Mutex
	unixLn   net.Listener
	tcpLn    net.Listener
	httpUnix *http.Server
	httpTCP  *http.Server
}

// ServerOption configures a Server at construction.
type ServerOption func(*Server)

// WithVerifier sets the TCP PAT verifier. The daemon passes the store-backed one
// (verifier.go, over the meta PAT store); without this option the default is
// api.RejectAllVerifier, so every TCP request 401s -- the honest answer when no PAT
// store is wired. A nil verifier is ignored, keeping that safe default.
func WithVerifier(v api.TokenVerifier) ServerOption {
	return func(s *Server) {
		if v != nil {
			s.verifier = v
		}
	}
}

// WithServerLogger sets the Server's logger. The default discards output. A nil
// logger is ignored.
func WithServerLogger(l *slog.Logger) ServerOption {
	return func(s *Server) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewServer builds a Server serving handler on the listeners settings selects.
// By default the TCP listener (if any) rejects every token (the honest no-PAT
// state) and log output is discarded; override with WithVerifier and
// WithServerLogger.
func NewServer(settings config.Settings, handler http.Handler, opts ...ServerOption) *Server {
	s := &Server{
		settings: settings,
		handler:  handler,
		verifier: api.RejectAllVerifier(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start binds the listeners and begins serving in background goroutines,
// returning once both are bound (or an error if a bind fails). The unix socket
// is always bound; the TCP listener is bound only when settings.TCP is set. It
// prepares the socket directory and clears a stale socket first.
func (s *Server) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := PrepareSocketDir(s.settings); err != nil {
		return err
	}
	unixLn, err := listenUnix(s.settings.Socket)
	if err != nil {
		return err
	}
	s.unixLn = unixLn
	// The unix socket is ambient (local, filesystem-guarded): it serves the mux
	// directly, with no PAT gate.
	s.httpUnix = newHTTPServer(s.handler)
	go serve(s.httpUnix, unixLn, s.logger, "unix")

	if s.settings.TCP != "" {
		tcpLn, err := s.listenTCP()
		if err != nil {
			_ = s.closeLocked()
			return err
		}
		s.tcpLn = tcpLn
		// Every TCP request must present a PAT: wrap the shared mux with the gate.
		s.httpTCP = newHTTPServer(api.RequirePAT(s.verifier, s.handler))
		go serve(s.httpTCP, tcpLn, s.logger, "tcp")
	}
	return nil
}

// listenUnix binds the unix control socket and clamps its file permissions to
// owner-only, the filesystem guard on the local-only control plane.
func listenUnix(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on unix socket %s: %w", path, err)
	}
	if err := os.Chmod(path, socketFilePerm); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon: restrict socket permissions %s: %w", path, err)
	}
	return ln, nil
}

// listenTCP binds the TCP listener and, when a cert/key pair is configured, wraps
// it in TLS; absent certs, it is plain TCP (no certs: plain TCP).
func (s *Server) listenTCP() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.settings.TCP)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on TCP %s: %w", s.settings.TCP, err)
	}
	if s.settings.TLSCert != "" && s.settings.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(s.settings.TLSCert, s.settings.TLSKey)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("daemon: load TLS cert/key: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
	}
	return ln, nil
}

// newHTTPServer builds a hardened http.Server for a listener.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: serverReadHeaderTimeout}
}

// serve runs srv.Serve on ln, logging any error that is not the normal
// close-triggered ErrServerClosed.
func serve(srv *http.Server, ln net.Listener, log *slog.Logger, which string) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("iris daemon listener stopped", "listener", which, "err", err)
	}
}

// Serve blocks until ctx is cancelled (SIGTERM/SIGINT in a foreground daemon),
// then shuts the listeners down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	<-ctx.Done()
	return s.Shutdown()
}

// Shutdown gracefully drains both listeners and removes the socket file. It is
// idempotent and safe to call from a test cleanup.
func (s *Server) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

// closeLocked shuts both http servers down within the grace period and clears the
// socket file. The caller holds s.mu.
func (s *Server) closeLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), serverShutdownGrace)
	defer cancel()

	var errs []error
	if s.httpUnix != nil {
		if err := s.httpUnix.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("daemon: shut down unix listener: %w", err))
		}
		s.httpUnix = nil
	}
	if s.httpTCP != nil {
		if err := s.httpTCP.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("daemon: shut down TCP listener: %w", err))
		}
		s.httpTCP = nil
	}
	s.unixLn = nil
	s.tcpLn = nil
	// The unix listener unlinks its socket on close; remove any straggler so a
	// restart binds cleanly.
	if err := os.Remove(s.settings.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("daemon: remove socket %s: %w", s.settings.Socket, err))
	}
	return errors.Join(errs...)
}

// SocketPath returns the unix control socket the server listens on.
func (s *Server) SocketPath() string { return s.settings.Socket }

// TCPEnabled reports whether the optional TCP listener is up.
func (s *Server) TCPEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcpLn != nil
}

// TCPAddr returns the resolved TCP listener address (useful when the configured
// address used port 0), or "" when no TCP listener is up.
func (s *Server) TCPAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tcpLn == nil {
		return ""
	}
	return s.tcpLn.Addr().String()
}
