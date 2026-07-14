// Package socketio is a real socket-HTTP test harness: an in-process net/http
// server bound to a unix-domain socket in a throwaway temp dir, paired with an
// http.Client that dials that socket. It proves the unix-socket control plane
// for real -- real listener, real HTTP framing, real socket I/O -- with no
// database.
//
// It fixes the socket convention E02's daemon control plane and the CLI's socket
// client reuse: the default CLI-daemon channel is an always-on, local-only unix
// socket carrying HTTP/JSON. This is test-support infrastructure imported only
// by _test.go files.
package socketio

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Server is an in-process HTTP server bound to a unix socket. Path is the socket
// on disk; Client dials it. Serve registers cleanup on the test, so callers need
// not close it explicitly.
type Server struct {
	// Path is the unix socket the server listens on, inside the test's temp dir.
	Path string
	// Client is an http.Client whose transport dials Path; request URLs use any
	// host (it is ignored by the dialer), e.g. "http://iris/healthz".
	Client *http.Client

	srv *http.Server
	ln  net.Listener
	dir string
}

// Serve starts an in-process HTTP server for h on a unix socket in a fresh temp
// dir and returns a Server whose Client dials it. The server and socket are torn
// down automatically via t.Cleanup.
//
// The temp dir is created short (not t.TempDir, which embeds the long test name)
// so the socket path stays under the platform's tight sockaddr_un limit (104
// bytes on darwin).
func Serve(t testing.TB, h http.Handler) *Server {
	t.Helper()

	dir, err := os.MkdirTemp("", "iris")
	if err != nil {
		t.Fatalf("socketio: temp dir: %v", err)
	}
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("socketio: listen on unix socket %s: %v", path, err)
	}

	s := &Server{
		Path: path,
		// ReadHeaderTimeout bounds header reads (Slowloris guard); the harness is
		// a real HTTP server, so it carries the same hardening the engine's
		// listener will.
		srv: &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second},
		ln:  ln,
		dir: dir,
	}
	s.Client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}

	go func() {
		// Serve returns ErrServerClosed on Close; that is the normal stop path.
		_ = s.srv.Serve(ln)
	}()
	t.Cleanup(s.Close)
	return s
}

// Close shuts the server down and releases the socket. It is idempotent and is
// registered as the test cleanup by Serve.
func (s *Server) Close() {
	s.Client.CloseIdleConnections()
	_ = s.srv.Close()
	_ = s.ln.Close()
	_ = os.RemoveAll(s.dir)
}
