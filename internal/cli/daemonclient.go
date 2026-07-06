package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// daemonProbeTimeout bounds the reachability probe so a daemon-touching command
// fails fast (never hangs) when nothing is listening.
const daemonProbeTimeout = 3 * time.Second

// requireDaemon is the reachability gate every daemon-touching command passes
// through: it resolves the configured target and dials it. A refused or absent
// daemon is no-daemon (exit 3) with start guidance, never an auto-start. A
// reachable daemon lets the command proceed; since the command bodies land in
// later epics, a reached daemon currently yields not-implemented (exit 4).
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

	client, base, overTCP := daemonHTTPClient(s)
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

// daemonHTTPClient builds the HTTP client and base URL for the resolved target:
// a plain TCP client for a configured host, otherwise a unix-socket dialer for
// the control socket. overTCP reports whether the target is the TCP host (so the
// caller knows to attach a PAT).
func daemonHTTPClient(s config.Settings) (client *http.Client, base string, overTCP bool) {
	if s.Host != "" {
		return &http.Client{}, "http://" + s.Host, true
	}
	socket := s.Socket
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}, "http://iris", false
}
