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

// This file is the CLI side of `iris deadletter replay` (specification sections 6.2
// and 8). Replay is a leader-owned disposition: the CLI resolves and validates the
// scope locally (a bare invocation is a usage error, exit 2 -- nothing defaults to
// everything), then POSTs the scope to the daemon's leader-gated replay route. The
// leader resolves the worklist to its root causes, mints each a fresh run on current
// data (cause replay, replayed_from the replaced run), and reports the outcome. The
// exit code follows the section-8 categories: success 0, a replay that itself
// dead-lettered 5, no daemon reachable 3, not the leader 6, any other failure 4.
//
// Drain (`iris deadletter drain`, below) mirrors the same shape: the CLI
// resolves/validates the scope locally (bare is a usage error, exit 2), then POSTs
// it to the daemon's leader-gated drain route. Drain is a pure discard
// (specification sections 6.2 and 12): the leader resolves the scope to the exact
// entries named -- never walking failed_upstream to a root the way replay does --
// and deletes their dead_letters worklist rows; nothing re-runs, nothing downstream
// is altered. There is no re-dead-letter outcome for drain (nothing runs, so
// nothing can dead-letter again), so a clean drain always exits 0; the confirmation
// gate for this destructive op (typed-name prompt, soft-block y/N) is E10.2's
// contract, not this one's -- --yes/--force are accepted and forwarded, not yet
// locally gated.

// replayScope is the POST /deadletter/replay body: the operator scope, exactly one of
// a single run, one pipeline's entries, or every outstanding entry. The leader
// resolves it to distinct root causes before minting replacements.
type replayScope struct {
	// Run is the single dead-lettered run to replay (empty unless the <run> form).
	Run string `json:"run,omitempty"`
	// Pipeline scopes to one pipeline's outstanding entries (--pipeline).
	Pipeline string `json:"pipeline,omitempty"`
	// All scopes to every outstanding entry (--all).
	All bool `json:"all,omitempty"`
}

// replayOutcome is the leader's reply: which replaced runs were replayed and which
// replays themselves dead-lettered again. A non-empty DeadLettered list is the
// exit-5 condition (specification section 6.2: a dead-lettering replay parks a fresh
// entry chained via replayed_from and exits 5).
type replayOutcome struct {
	// Replayed names each replaced run and the fresh replacement minted for it.
	Replayed []replayedRun `json:"replayed"`
	// DeadLettered names each replay whose replacement run itself dead-lettered again.
	DeadLettered []replayedRun `json:"dead_lettered"`
}

// replayedRun pairs a replaced dead-lettered run with the fresh replacement minted
// for it; ReplayedFrom is the replacement's replay lineage (the replaced run).
type replayedRun struct {
	// ReplacedRun is the dead-lettered run that was replayed (its worklist entry
	// cleared when the replacement minted).
	ReplacedRun string `json:"replaced_run"`
	// ReplacementRun is the fresh run minted on current data.
	ReplacementRun string `json:"replacement_run"`
	// ReplayedFrom is the replacement's runs.replayed_from (the replaced run).
	ReplayedFrom string `json:"replayed_from,omitempty"`
}

// deadletterReplay is the handler for `iris deadletter replay`. It requires an
// explicit scope (bare is a usage error), then POSTs the scope to the leader's replay
// route and maps the outcome to a section-8 exit category.
func (a *app) deadletterReplay() runE {
	return func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		pipeline, _ := cmd.Flags().GetString("pipeline")
		var run string
		if len(args) == 1 {
			run = args[0]
		}
		// Nothing defaults to everything: a bare replay is a usage error (exit 2).
		if run == "" && pipeline == "" && !all {
			return a.usage("deadletter replay requires <run>, --pipeline <name>, or --all")
		}
		return a.postReplay(cmd, replayScope{Run: run, Pipeline: pipeline, All: all})
	}
}

// postReplay sends the replay scope to the leader and classifies the response. A
// transport failure is no-daemon (exit 3) with start guidance; every other outcome is
// classified by classifyReplayResponse.
func (a *app) postReplay(cmd *cobra.Command, scope replayScope) error {
	settings := a.resolveTarget(cmd)
	client, base, overTCP := a.daemonHTTPClient(settings)

	body, err := json.Marshal(scope)
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "encode", message: fmt.Sprintf("deadletter replay: encode request: %v", err)}
	}
	hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+"/deadletter/replay", bytes.NewReader(body))
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("deadletter replay: build request: %v", err)}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if overTCP && settings.Token != "" {
		hreq.Header.Set("Authorization", "Bearer "+settings.Token)
	}

	resp, err := client.Do(hreq)
	if err != nil {
		a.logger.Debug("no iris daemon reachable", "op", "deadletter replay", "socket", settings.Socket, "host", settings.Host, "err", err)
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
	return a.classifyReplayResponse(cmd, resp)
}

// classifyReplayResponse maps the leader's replay reply to a command outcome. A 200
// with no re-dead-letter is success (exit 0); a 200 whose outcome names a replay that
// dead-lettered again is exit 5 (the dead-lettering-replay contract); a not_leader
// status is exit 6 naming the leader; any other status is operation-failed (exit 4).
func (a *app) classifyReplayResponse(cmd *cobra.Command, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data replayOutcome `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("deadletter replay: decode daemon response: %v", err)}
		}
		if len(env.Data.DeadLettered) > 0 {
			return a.replayDeadLettered(env.Data)
		}
		return a.emitReplaySuccess(cmd, env.Data)
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
			msg = fmt.Sprintf("deadletter replay failed (daemon status %d)", resp.StatusCode)
		}
		return &fault{code: exitOpFailed, codeStr: code, message: msg}
	}
}

// replayDeadLettered is the exit-5 outcome: at least one replayed run dead-lettered
// again, parking a fresh entry chained via replayed_from. It names the re-dead-lettered
// runs so the operator can act, and carries the dead-lettered exit category.
func (a *app) replayDeadLettered(out replayOutcome) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "replay dead-lettered again: ")
	for i, r := range out.DeadLettered {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "run %s (replaces %s)", r.ReplacementRun, r.ReplacedRun)
	}
	return &fault{code: exitDeadLettered, codeStr: "dead_lettered", message: b.String()}
}

// emitReplaySuccess renders a clean replay (exit 0): a single JSON data envelope
// under --json, otherwise a human line per replayed run on stdout.
func (a *app) emitReplaySuccess(cmd *cobra.Command, out replayOutcome) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: out})
	}
	if len(out.Replayed) == 0 {
		fmt.Fprintln(a.out, "no outstanding entries to replay")
		return nil
	}
	for _, r := range out.Replayed {
		fmt.Fprintf(a.out, "replayed %s as %s\n", r.ReplacedRun, r.ReplacementRun)
	}
	return nil
}

// drainScope is the POST /deadletter/drain body: the operator scope, exactly one of
// a single run, one pipeline's outstanding entries, or every outstanding entry. The
// leader resolves it to the exact scoped dead_letters run ids (never walking
// failed_upstream to a root the way replay's scope does) and discards them.
type drainScope struct {
	// Run is the single dead-lettered run to drain (empty unless the <run> form).
	Run string `json:"run,omitempty"`
	// Pipeline scopes to one pipeline's outstanding entries (--pipeline).
	Pipeline string `json:"pipeline,omitempty"`
	// All scopes to every outstanding entry (--all).
	All bool `json:"all,omitempty"`
}

// drainOutcome is the leader's reply: the dead-lettered runs whose worklist entries
// were discarded. Every drained run becomes prunable; none of them can be replayed
// again -- their dead_letters entry, the only replay ticket, is gone.
type drainOutcome struct {
	// Drained names each run whose dead_letters entry was discarded.
	Drained []string `json:"drained"`
}

// deadletterDrain is the handler for `iris deadletter drain`. It requires an
// explicit scope (bare is a usage error, exit 2 -- specification sections 6.2, 8,
// and 12: nothing defaults to --all), then POSTs the scope to the leader's drain
// route and maps the outcome to a section-8 exit category.
func (a *app) deadletterDrain() runE {
	return func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		pipeline, _ := cmd.Flags().GetString("pipeline")
		var run string
		if len(args) == 1 {
			run = args[0]
		}
		// Nothing defaults to everything: a bare drain is a usage error (exit 2).
		if run == "" && pipeline == "" && !all {
			return a.usage("deadletter drain requires <run>, --pipeline <name>, or --all")
		}
		return a.postDrain(cmd, drainScope{Run: run, Pipeline: pipeline, All: all})
	}
}

// postDrain sends the drain scope to the leader and classifies the response. A
// transport failure is no-daemon (exit 3) with start guidance; every other outcome is
// classified by classifyDrainResponse.
func (a *app) postDrain(cmd *cobra.Command, scope drainScope) error {
	settings := a.resolveTarget(cmd)
	client, base, overTCP := a.daemonHTTPClient(settings)

	body, err := json.Marshal(scope)
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "encode", message: fmt.Sprintf("deadletter drain: encode request: %v", err)}
	}
	hreq, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, base+"/deadletter/drain", bytes.NewReader(body))
	if err != nil {
		return &fault{code: exitOpFailed, codeStr: "request", message: fmt.Sprintf("deadletter drain: build request: %v", err)}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if overTCP && settings.Token != "" {
		hreq.Header.Set("Authorization", "Bearer "+settings.Token)
	}

	resp, err := client.Do(hreq)
	if err != nil {
		a.logger.Debug("no iris daemon reachable", "op", "deadletter drain", "socket", settings.Socket, "host", settings.Host, "err", err)
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
	return a.classifyDrainResponse(cmd, resp)
}

// classifyDrainResponse maps the leader's drain reply to a command outcome. A 200 is
// success (exit 0) -- drain has no re-dead-letter category, since nothing it does
// ever re-runs; a not_leader status is exit 6 naming the leader; any other status is
// operation-failed (exit 4).
func (a *app) classifyDrainResponse(cmd *cobra.Command, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		var env struct {
			Data drainOutcome `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return &fault{code: exitOpFailed, codeStr: "decode", message: fmt.Sprintf("deadletter drain: decode daemon response: %v", err)}
		}
		return a.emitDrainSuccess(cmd, env.Data)
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
			msg = fmt.Sprintf("deadletter drain failed (daemon status %d)", resp.StatusCode)
		}
		return &fault{code: exitOpFailed, codeStr: code, message: msg}
	}
}

// emitDrainSuccess renders a clean drain (exit 0): a single JSON data envelope under
// --json, otherwise a human line per drained run on stdout.
func (a *app) emitDrainSuccess(cmd *cobra.Command, out drainOutcome) error {
	if jsonMode, _ := cmd.Flags().GetBool("json"); jsonMode {
		return json.NewEncoder(a.out).Encode(dataEnvelope{Data: out})
	}
	if len(out.Drained) == 0 {
		fmt.Fprintln(a.out, "no outstanding entries to drain")
		return nil
	}
	for _, r := range out.Drained {
		fmt.Fprintf(a.out, "drained %s\n", r)
	}
	return nil
}
