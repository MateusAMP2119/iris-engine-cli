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

// This file is the shared JSON-request half of the daemon-touching commands whose
// outcome is a data envelope rather than the control result shape
// (endpoint apply, pat create). It resolves the daemon target through the
// configuration precedence, POSTs the request (attaching the PAT over TCP), and
// classifies the response into the exit categories -- a transport failure
// is no-daemon (exit 3), a not_leader rejection is exit 6, any other error is
// operation-failed (exit 4), a 200 decodes the {"data": ...} envelope into out.

// postDaemonJSON POSTs req to the daemon route and, on success, decodes the data
// envelope into out. op names the operation for error messages. The classification
// mirrors postControl so every daemon-touching command maps daemon outcomes to the
// same exit categories.
func (a *app) postDaemonJSON(cmd *cobra.Command, route string, req any, op string, out any) error {
	settings := a.resolveTarget(cmd)
	client, base, overTCP := a.daemonHTTPClient(settings)

	body, err := json.Marshal(req)
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "encode", message: fmt.Sprintf("%s: encode request: %v", op, err)}
	}
	hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+route, bytes.NewReader(body))
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("%s: build request: %v", op, err)}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if overTCP && settings.Token != "" {
		hreq.Header.Set("Authorization", "Bearer "+settings.Token)
	}

	resp, err := client.Do(hreq)
	if err != nil {
		a.logger.Debug("no iris daemon reachable", "op", op, "socket", settings.Socket, "host", settings.Host, "err", err)
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

	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("%s: decode daemon response: %v", op, err)}
		}
		if out != nil {
			if err := json.Unmarshal(env.Data, out); err != nil {
				return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("%s: decode daemon response payload: %v", op, err)}
			}
		}
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
			msg = fmt.Sprintf("%s failed (daemon status %d)", op, resp.StatusCode)
		}
		return &fault{code: exitOpFailed, codeStr: code, message: msg}
	}
}
