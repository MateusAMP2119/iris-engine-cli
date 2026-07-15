package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// daemonProbeTimeout bounds the reachability probe so a daemon-touching command
// fails fast (never hangs) when nothing is listening.
const daemonProbeTimeout = 3 * time.Second

// requireDaemon is the reachability gate behind the command surface's remaining
// unwired verbs: it resolves the configured target and dials it. A refused or
// absent daemon is no-daemon (exit 3) with start guidance, never an auto-start.
// Reaching a daemon proves only that much: these verbs have no body of their own,
// so a reached daemon yields not-implemented (exit 4). The wired commands do not
// pass through here -- each dials and classifies its own route.
func (a *app) requireDaemon(cmd *cobra.Command, op string) error {
	target := a.resolveTarget(cmd)
	if err := a.probeDaemon(cmd.Context(), target); err != nil {
		a.logger.Debug("no iris daemon reachable", "op", op, "socket", target.Socket, "host", target.Host, "err", err)
		return &fault{
			code:    exitNoDaemon,
			codeStr: "no_daemon",
			message: `no Iris daemon reachable; start the engine with "iris engine start", or target a running daemon with --socket or --host`,
		}
	}
	return &fault{
		code:    exitOpFailed,
		codeStr: "not_implemented",
		message: op + " reached the daemon, but is not implemented yet",
	}
}

// probeDaemon dials the resolved daemon and issues GET /healthz, returning nil
// when the daemon answers 2xx and an error otherwise (connection refused, missing
// socket, non-2xx). It prefers a configured TCP host, else the unix socket, and
// presents the PAT over TCP when one is configured.
func (a *app) probeDaemon(ctx context.Context, s config.Settings) error {
	if ctx == nil {
		ctx = context.Background()
	}
	pctx, cancel := context.WithTimeout(ctx, daemonProbeTimeout)
	defer cancel()

	client, base, overTCP := a.daemonHTTPClient(s)
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	if overTCP && s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("daemon returned status %d from /healthz", resp.StatusCode)
	}
	return nil
}

// daemonHTTPClient builds the HTTP client and base URL for the resolved target.
// For a configured --host it is scheme-aware: an https:// host probes over TLS
// (standard verification against the system trust store, or the injected
// daemonTLSConfig in tests), while http:// or a bare host:port stays plain TCP --
// so a TLS-serving daemon is reached rather than failing the plain-HTTP
// handshake. With no host it dials the local unix socket. overTCP reports whether
// the target is the TCP host (so the caller knows to attach a PAT).
func (a *app) daemonHTTPClient(s config.Settings) (client *http.Client, base string, overTCP bool) {
	if s.Host != "" {
		scheme, hostport := hostScheme(s.Host)
		if scheme == "https" {
			return &http.Client{Transport: &http.Transport{TLSClientConfig: a.daemonTLSConfig}}, "https://" + hostport, true
		}
		return &http.Client{}, "http://" + hostport, true
	}
	socket := s.Socket
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}, "http://iris", false
}

// hostScheme splits a --host value into its transport scheme and host:port. An
// explicit https:// selects TLS, http:// and a bare host:port stay plain (the
// documented default). The scheme prefix is stripped from the returned host:port.
func hostScheme(host string) (scheme, hostport string) {
	switch {
	case strings.HasPrefix(host, "https://"):
		return "https", strings.TrimPrefix(host, "https://")
	case strings.HasPrefix(host, "http://"):
		return "http", strings.TrimPrefix(host, "http://")
	default:
		return "http", host
	}
}
