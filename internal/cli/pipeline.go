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

// This file is the CLI side of iris pipeline run and iris pipeline list. run is
// a control mutation POSTed to the leader-gated /pipeline/run route;
// its business outcome rides the response body's state field, which the CLI maps to an
// exit-code category: queued or succeeded is success (0), ineligible is operation-failed
// (4) with the gate reason, dead-lettered is exit 5. list is a read GET to
// /pipeline/list (any node): active-only by default, every registered pipeline with
// --all. Transport failure is no-daemon (3) with start guidance; a not_leader rejection
// on the mutation is exit 6 with leader guidance.

// pipelineRun is the handler for `iris pipeline run <name>`: it POSTs the run request to
// the daemon and maps the outcome to an exit code.
func (a *app) pipelineRun() runE {
	return func(cmd *cobra.Command, args []string) error {
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		body, err := json.Marshal(api.PipelineRunRequest{Pipeline: args[0]})
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "encode", message: fmt.Sprintf("pipeline run: encode request: %v", err)}
		}
		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+"/pipeline/run", bytes.NewReader(body))
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("pipeline run: build request: %v", err)}
		}
		hreq.Header.Set("Content-Type", "application/json")
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "pipeline run", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
		return a.classifyPipelineRun(cmd, resp)
	}
}

// classifyPipelineRun maps a /pipeline/run response to a command outcome. A 200 carries
// the manual-run business outcome in its state field: queued or succeeded is success,
// ineligible is exit 4 with the gate reason, dead-lettered is exit 5. A not_leader status
// is exit 6 naming the leader; every other status is operation-failed (exit 4).
func (a *app) classifyPipelineRun(cmd *cobra.Command, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data api.PipelineRunResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("pipeline run: decode daemon response: %v", err)}
		}
		return a.pipelineRunOutcome(cmd, env.Data)
	case api.StatusNotLeader:
		return a.notLeaderFault(resp)
	default:
		return a.controlFailure(resp, "pipeline run")
	}
}

// pipelineRunOutcome renders a successful /pipeline/run call by its state: queued and
// succeeded are success (exit 0, emitted as a data envelope under --json), ineligible is
// operation-failed (exit 4) carrying the gate reason, and dead-lettered is exit 5.
func (a *app) pipelineRunOutcome(cmd *cobra.Command, res api.PipelineRunResult) error {
	switch res.State {
	case api.PipelineRunQueued, api.PipelineRunSucceeded:
		if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
		}
		fmt.Fprintf(a.out, "%s: %s\n", res.Pipeline, res.State)
		return nil
	case api.PipelineRunIneligible:
		msg := res.Reason
		if msg == "" {
			msg = fmt.Sprintf("%s is not eligible to run", res.Pipeline)
		}
		return &fault{code: exitOpFailed, codeStr: "ineligible", message: msg}
	case api.PipelineRunDeadLettered:
		return &fault{code: exitDeadLettered, codeStr: "dead_lettered",
			message: fmt.Sprintf("run of %s dead-lettered; inspect it with \"iris deadletter show %s\"", res.Pipeline, res.Pipeline)}
	default:
		return &fault{code: exitOpFailed, codeStr: "operation_failed",
			message: fmt.Sprintf("pipeline run returned an unknown state %q", res.State)}
	}
}

// pipelineList is the handler for `iris pipeline list [--all]`: it GETs the pipeline
// listing (active-only by default, every registered pipeline with --all) and renders it.
func (a *app) pipelineList() runE {
	return func(cmd *cobra.Command, _ []string) error {
		all, _ := cmd.Flags().GetBool("all")
		settings := a.resolveTarget(cmd)
		client, base, overTCP := a.daemonHTTPClient(settings)

		url := base + "/pipeline/list"
		if all {
			url += "?all=1"
		}
		hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, url, nil)
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("pipeline list: build request: %v", err)}
		}
		if overTCP && settings.Token != "" {
			hreq.Header.Set("Authorization", "Bearer "+settings.Token)
		}

		resp, err := client.Do(hreq)
		if err != nil {
			a.logger.Debug("no iris daemon reachable", "op", "pipeline list", "socket", settings.Socket, "host", settings.Host, "err", err)
			return a.noDaemonFault()
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			return a.controlFailure(resp, "pipeline list")
		}
		var env struct {
			Data api.PipelineListResult `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("pipeline list: decode daemon response: %v", err)}
		}
		return a.emitPipelineList(cmd, env.Data, all)
	}
}

// emitPipelineList renders the listing: a single JSON data envelope under --json, else a
// human list on stdout -- one pipeline per line, active ones marked, and a friendly line
// when the default (active-only) view is empty.
func (a *app) emitPipelineList(cmd *cobra.Command, res api.PipelineListResult, all bool) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: res})
	}
	if len(res.Pipelines) == 0 {
		if all {
			fmt.Fprintln(a.out, "no pipelines registered")
		} else {
			fmt.Fprintln(a.out, "no pipelines with a queued or running run")
		}
		return nil
	}
	for _, p := range res.Pipelines {
		state := "idle"
		if p.Active {
			state = "active"
		}
		fmt.Fprintf(a.out, "%s\t%s\n", p.Name, state)
	}
	return nil
}

// noDaemonFault is the shared no-daemon (exit 3) outcome with start guidance, folded into
// the message so it rides both human output and the --json envelope.
func (a *app) noDaemonFault() error {
	return &fault{
		code:    exitNoDaemon,
		codeStr: "no_daemon",
		message: `no Iris daemon reachable; start the engine with "iris engine start", or target a running daemon with --socket or --host`,
	}
}

// notLeaderFault maps a not_leader control response to exit 6 with leader guidance.
func (a *app) notLeaderFault(resp *http.Response) error {
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
}

// controlFailure maps a non-200, non-not-leader control response to operation-failed
// (exit 4), carrying the daemon's message when it sent one.
func (a *app) controlFailure(resp *http.Response, op string) error {
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
