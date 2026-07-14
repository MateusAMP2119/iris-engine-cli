package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// This file is the CLI side of the declare apply/destroy control mutations. Both
// commands resolve and validate the single target declaration locally (fast,
// actionable feedback: a bad target is operation-failed, exit 4, before any network),
// then send the workspace-relative path to the daemon's leader-gated control route.
// The leader resolves the declaration and the schemas/ tree against its own workspace
// tree and runs the registry apply plus schema provisioning (apply) or the scoped
// teardown (destroy). Exit codes follow the standard categories: success 0, no daemon
// reachable 3, operation failed 4, not the leader 6; any advisory warnings ride the
// terminal envelope.

// declareApply is the handler for `iris declare apply`. It resolves and parses the
// single target declaration, computes the advisory warnings the apply surfaces
// (cross-mode reads and the like) through the applyWarnings seam, then POSTs the
// target to the daemon's /apply route. The warnings accompany the apply, never replace
// it: they ride the terminal --json envelope whether the apply succeeds or fails.
func (a *app) declareApply() runE {
	return func(cmd *cobra.Command, args []string) error {
		_, decl, err := declare.LoadDeclarationFile(args[0])
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "declare_target", message: err.Error()}
		}
		if a.applyWarnings != nil {
			a.warnings = a.applyWarnings(decl)
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		return a.postControl(cmd, "/apply", api.ControlRequest{Path: args[0], DryRun: dryRun}, "declare apply")
	}
}

// declareDestroy is the handler for `iris declare destroy`. It resolves and parses the
// single target declaration locally (a bad target is exit 4), enforces the
// confirmation gate (typed-name on TTY for this teardown, or --yes/--force), then
// POSTs to the daemon's /destroy with the confirm flag the API requires.
func (a *app) declareDestroy() runE {
	return func(cmd *cobra.Command, args []string) error {
		if _, _, err := declare.LoadDeclarationFile(args[0]); err != nil {
			return &fault{code: exitOpFailed, codeStr: "declare_target", message: err.Error()}
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		// Local confirmation gate for teardown (typed-name via seam or flags).
		// Use the provided path as the name for typed confirmation.
		targetName := args[0]
		confirmed, cerr := a.confirmOrFlags(cmd, targetName, true)
		if cerr != nil {
			return cerr
		}
		yes, _ := cmd.Flags().GetBool("yes")
		force, _ := cmd.Flags().GetBool("force")
		if !confirmed && !yes && !force {
			return &fault{
				code:    exitOpFailed,
				codeStr: "confirmation_required",
				message: "declare destroy is an irreversible teardown; re-run with --yes or --force, or type the target name to confirm",
			}
		}
		return a.postControl(cmd, "/destroy", api.ControlRequest{Path: args[0], DryRun: dryRun, Confirm: true, Force: force}, "declare destroy")
	}
}

// postControl sends a control mutation to the daemon and maps its outcome to an exit
// category. It resolves the daemon target through the configuration precedence, POSTs
// the request (attaching the PAT over TCP), and classifies the response: a transport
// failure is no-daemon (exit 3) with start guidance; a not_leader rejection is exit 6
// with leader guidance; any other error is operation-failed (exit 4); a 200 is success,
// emitted with any warnings.
func (a *app) postControl(cmd *cobra.Command, route string, req api.ControlRequest, op string) error {
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
	return a.classifyControlResponse(cmd, resp, op)
}

// classifyControlResponse maps a control-route HTTP response to a command outcome. A
// 200 emits the success result (with warnings); a not_leader status is exit 6 naming
// the leader; every other status is operation-failed (exit 4) carrying the daemon's
// message.
func (a *app) classifyControlResponse(cmd *cobra.Command, resp *http.Response, op string) error {
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data api.ControlResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("%s: decode daemon response: %v", op, err)}
		}
		return a.emitControlSuccess(cmd, env.Data)
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

// emitControlSuccess renders a successful control mutation: a single JSON data
// envelope under --json (carrying the result and any warnings), otherwise a human line
// on stdout with warnings on stderr. The warnings combine the local advisory (the
// applyWarnings seam) with any the daemon returned.
func (a *app) emitControlSuccess(cmd *cobra.Command, res api.ControlResult) error {
	warnings := append([]declare.Warning(nil), a.warnings...)
	for _, w := range res.Warnings {
		warnings = append(warnings, declare.Warning{Message: w})
	}

	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(controlSuccessEnvelope{Data: res, Warnings: warnings})
	}
	for _, w := range warnings {
		fmt.Fprintf(a.errOut, "iris: warning: %s\n", w.Message)
	}
	verb := "applied"
	if res.DryRun {
		verb = "would apply"
	}
	fmt.Fprintf(a.out, "%s %s %s\n", verb, res.Kind, res.Target)
	return nil
}

// controlSuccessEnvelope is the --json success document for a control mutation: the
// data envelope plus any advisory warnings that rode the outcome, so a successful
// apply/destroy emits one JSON document carrying both.
type controlSuccessEnvelope struct {
	Data     api.ControlResult `json:"data"`
	Warnings []declare.Warning `json:"warnings,omitempty"`
}
