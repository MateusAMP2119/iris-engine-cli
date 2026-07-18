package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/plugin"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file is the daemon side of #215's call path: resolving a turn's declared
// plugins (digest pin; a failure refuses the run) and servicing one verb call by
// exec'ing the tool binary with scrubbed env, confined workdir, per-verb timeout.

// pluginCaller services one declared-plugin verb call; it must always answer.
type pluginCaller interface {
	// Call runs one verb and returns its JSON result, or the failure that
	// becomes the err res frame.
	Call(ctx context.Context, call dispatch.TurnCall) (json.RawMessage, error)
}

// resolvedPlugins is one turn's digest-verified plugin set.
type resolvedPlugins struct {
	// calls is the declared enforcement surface the collector reads.
	calls dispatch.CallSet
	// pins are the run_plugins rows the run records.
	pins []store.RunPluginPin
	// caller services the turn's calls.
	caller pluginCaller
}

// resolveTurnPlugins resolves and digest-verifies every declared binding for
// one turn; any failure (not installed, digest deviation, unsupported kind or
// lifetime) refuses the run. stderr receives the plugins' stderr (the turn
// log); runner execs the per-call subprocesses. nil out means no plugins declared.
func resolveTurnPlugins(root string, declared map[string]declare.PluginUse, runner exec.Runner, stderr io.Writer) (*resolvedPlugins, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	aliases := make([]string, 0, len(declared))
	for alias := range declared {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	rp := &resolvedPlugins{calls: dispatch.CallSet{}}
	resolved := map[string]*plugin.Resolved{}
	for _, alias := range aliases {
		use := declared[alias]
		if use.EffectiveLifetime() != declare.LifetimeRun {
			return nil, fmt.Errorf("plugin %q lifetime %q is not yet supported; tool plugins run per call", alias, use.EffectiveLifetime())
		}
		ref, err := plugin.ParseRef(use.Ref)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: %w", alias, err)
		}
		res, err := plugin.Resolve(root, ref)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: %w", alias, err)
		}
		if res.Manifest.Kind != plugin.KindTool {
			return nil, fmt.Errorf("plugin %q is kind %q; only tool plugins are supported today", alias, res.Manifest.Kind)
		}
		verbs := map[string]bool{}
		for verb := range res.Manifest.Verbs {
			verbs[verb] = true
		}
		rp.calls[alias] = verbs
		resolved[alias] = res
		rp.pins = append(rp.pins, store.RunPluginPin{Alias: alias, Name: ref.Name, Version: ref.Version, Digest: res.Digest})
	}
	rp.caller = &toolCaller{runner: runner, resolved: resolved, stderr: stderr}
	return rp, nil
}

// maxPluginResultBytes bounds one call's stdout result; large payloads belong
// in files (a later stage), never inline.
const maxPluginResultBytes = 1 << 20

// toolCaller execs a resolved tool binary per call: argv [binary, verb], args
// JSON on stdin, result JSON on stdout, stderr into the turn log. Scrubbed
// (empty) env, per-call temp workdir, per-verb timeout; a crash, timeout, or
// malformed result is an error the driver answers with an err res frame.
type toolCaller struct {
	runner   exec.Runner
	resolved map[string]*plugin.Resolved
	stderr   io.Writer
}

// Call services one verb call to completion.
func (t *toolCaller) Call(ctx context.Context, call dispatch.TurnCall) (json.RawMessage, error) {
	res, ok := t.resolved[call.Alias]
	if !ok {
		return nil, fmt.Errorf("plugin %q is not resolved for this run", call.Alias)
	}
	timeout := time.Duration(res.Manifest.Verbs[call.Verb].Timeout()) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "iris-plugin-*")
	if err != nil {
		return nil, fmt.Errorf("plugin %q: workdir: %w", call.Alias, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	args := call.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var out bytes.Buffer
	h, err := t.runner.Start(ctx, exec.Spec{
		Dir:    dir,
		Argv:   []string{res.Binary, call.Verb},
		Env:    []string{}, // scrubbed by design: a plugin sees nothing of the daemon's environment
		Stdout: &out,
		Stderr: t.stderr,
		Stdin:  bytes.NewReader(args),
	})
	if err != nil {
		return nil, fmt.Errorf("plugin %q: start: %w", call.Alias, err)
	}
	status, werr := h.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("plugin %q verb %q timed out after %s", call.Alias, call.Verb, timeout)
	}
	if werr != nil {
		return nil, fmt.Errorf("plugin %q verb %q: %w", call.Alias, call.Verb, werr)
	}
	if status.Signaled {
		return nil, fmt.Errorf("plugin %q verb %q was killed (signal %d)", call.Alias, call.Verb, status.Signal)
	}
	if status.Code != 0 {
		return nil, fmt.Errorf("plugin %q verb %q exited %d", call.Alias, call.Verb, status.Code)
	}
	raw := bytes.TrimSpace(out.Bytes())
	if len(raw) == 0 {
		return nil, fmt.Errorf("plugin %q verb %q wrote no result", call.Alias, call.Verb)
	}
	if len(raw) > maxPluginResultBytes {
		return nil, fmt.Errorf("plugin %q verb %q result exceeds %d bytes; large payloads belong in files", call.Alias, call.Verb, maxPluginResultBytes)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("plugin %q verb %q result is not JSON: %q", call.Alias, call.Verb, truncateForDetail(string(raw)))
	}
	return json.RawMessage(raw), nil
}

// truncateForDetail bounds a quoted result snippet for error detail.
func truncateForDetail(s string) string {
	const maxLen = 256
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return strings.TrimSpace(s)
}
