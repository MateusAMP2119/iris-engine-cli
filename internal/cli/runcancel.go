package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// This file is the CLI side of `iris run cancel <run>` (specification section 6.3:
// only an operator cancel frees a hung run). Cancel is a leader-owned disposition: the
// CLI validates the run ref locally, then POSTs it to the daemon's leader-gated cancel
// route. The leader kills the run's process group and dead-letters it as stopped. The
// exit code follows the section-8 categories: success 0, no daemon reachable 3, not the
// leader 6, any other failure (unknown or already-terminal run) 4.

// runCancelReq is the POST /run/cancel body: the run to cancel.
type runCancelReq struct {
	// Run is the running run to cancel.
	Run string `json:"run"`
}

// runCancelResult is the leader's reply: the cancelled run and its resulting terminal
// state.
type runCancelResult struct {
	// Run is the cancelled run id.
	Run string `json:"run"`
	// State is the run's resulting terminal state (dead_lettered).
	State string `json:"state"`
}

// runCancel is the handler for `iris run cancel <run>`. It validates the run ref, then
// POSTs it to the leader's cancel route and maps the outcome to a section-8 exit
// category.
func (a *app) runCancel() runE {
	return func(cmd *cobra.Command, args []string) error {
		run := args[0]
		if _, _, perr := parseRunRef(run); perr != nil {
			return a.usage(fmt.Sprintf("bad run ref %q: %v", run, perr))
		}
		return a.postRunCancel(cmd, runCancelReq{Run: run})
	}
}

// postRunCancel sends the cancel request to the leader and classifies the response. A
// transport failure is no-daemon (exit 3) with start guidance; every other outcome is
// classified by classifyRunCancelResponse.
func (a *app) postRunCancel(cmd *cobra.Command, req runCancelReq) error {
	settings := a.resolveTarget(cmd)
	client, base, overTCP := a.daemonHTTPClient(settings)

	body, err := json.Marshal(req)
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "encode", message: fmt.Sprintf("run cancel: encode request: %v", err)}
	}
	hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+"/run/cancel", bytes.NewReader(body))
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("run cancel: build request: %v", err)}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if overTCP && settings.Token != "" {
		hreq.Header.Set("Authorization", "Bearer "+settings.Token)
	}

	resp, err := client.Do(hreq)
	if err != nil {
		a.logger.Debug("no iris daemon reachable", "op", "run cancel", "socket", settings.Socket, "host", settings.Host, "err", err)
		return &fault{
			code:    exitNoDaemon,
			codeStr: "no_daemon",
			message: `no Iris daemon reachable; start the engine with "iris engine start", or target a running daemon with --socket or --host`,
		}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return a.classifyRunCancelResponse(cmd, resp)
}

// classifyRunCancelResponse maps the leader's cancel reply to a command outcome. A 200
// is success (exit 0); a not_leader status is exit 6 naming the leader; any other
// status is operation-failed (exit 4).
func (a *app) classifyRunCancelResponse(cmd *cobra.Command, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data runCancelResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("run cancel: decode daemon response: %v", err)}
		}
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: env.Data})
		}
		fmt.Fprintf(a.out, "cancelled %s (%s)\n", env.Data.Run, env.Data.State)
		return nil
	case api.StatusNotLeader:
		var env struct {
			Error struct {
				Message string `json:"message"`
				Leader  string `json:"leader"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		msg := env.Error.Message
		if msg == "" {
			msg = "this daemon is not the leader"
		}
		if env.Error.Leader != "" {
			msg = fmt.Sprintf("%s; retry against the leader (%s)", msg, env.Error.Leader)
		}
		return &fault{code: exitNotLeader, codeStr: api.CodeNotLeader, message: msg}
	default:
		var env struct {
			Error errBody `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		code := env.Error.Code
		if code == "" {
			code = "operation_failed"
		}
		msg := env.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("run cancel failed (daemon status %d)", resp.StatusCode)
		}
		return &fault{code: exitOpFailed, codeStr: code, message: msg}
	}
}
