package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// plugins (digest pin; a failure refuses the run) and servicing verb calls. A
// tool binding execs its binary per call (scrubbed env, confined workdir,
// per-verb timeout); a service binding relays the call to a supervised
// long-running instance (pluginservice.go) whose id the run's pin records. A
// result past the inline cap is spilled: the engine writes the body to a
// content-addressed payload file and the reply carries the path plus digest,
// so a large payload never rides the run's stdin.

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
	// pins are the run_plugins rows the run records (service pins carry the
	// attached instance id).
	pins []store.RunPluginPin
	// caller services the turn's calls.
	caller pluginCaller
	// cleanup ends the turn's run-lifetime service instances; nil when none.
	cleanup func()
}

// end runs the resolution's cleanup, if any. Nil-receiver-safe so every turn
// path can defer it unconditionally.
func (rp *resolvedPlugins) end() {
	if rp != nil && rp.cleanup != nil {
		rp.cleanup()
	}
}

// serviceScope is where a turn's service instances attach: the pipeline (an
// own-lane pipeline is a lane of one), its persisted lane, and the payload
// spill directory for oversized results.
type serviceScope struct {
	// Pipeline is the running pipeline's name.
	Pipeline string
	// Lane is the pipeline's persisted lane; empty means its own lane.
	Lane string
	// SpillDir is where oversized results are written; empty disables spilling
	// (an oversized result then fails the call).
	SpillDir string
}

// laneKey is the sharing key of a lane-lifetime instance: the persisted lane,
// or the pipeline itself for an own-lane pipeline (a lane of one).
func (s serviceScope) laneKey() string {
	if s.Lane != "" {
		return s.Lane
	}
	return s.Pipeline
}

// resolveTurnPlugins resolves and digest-verifies every declared binding for
// one turn; any failure (not installed, digest deviation, unsupported kind or
// lifetime) refuses the run. Tool bindings exec per call and must be
// run-lifetime; service bindings attach a supervised instance per their
// lifetime -- run (spawned for this turn, ended with it), lane (shared across
// the lane's serial walk), or resident (shared engine-wide across runs) -- and
// fresh replaces a shared instance with a cold one before the run attaches.
// stderr receives the plugins' stderr (the turn log); runner execs the
// per-call tool subprocesses; services supervises the instances (nil refuses
// service bindings: the shape-test composition has no supervisor). nil out
// means no plugins declared.
func resolveTurnPlugins(root string, declared map[string]declare.PluginUse, runner exec.Runner, stderr io.Writer, services *pluginServices, scope serviceScope) (*resolvedPlugins, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	aliases := make([]string, 0, len(declared))
	for alias := range declared {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	rp := &resolvedPlugins{calls: dispatch.CallSet{}}
	tools := map[string]*plugin.Resolved{}
	byAlias := map[string]pluginCaller{}
	var runInstances []*serviceSession
	// A resolve failure below abandons any run-lifetime instance already spawned.
	fail := func(err error) (*resolvedPlugins, error) {
		for _, s := range runInstances {
			s.end()
		}
		return nil, err
	}
	for _, alias := range aliases {
		use := declared[alias]
		ref, err := plugin.ParseRef(use.Ref)
		if err != nil {
			return fail(fmt.Errorf("plugin %q: %w", alias, err))
		}
		res, err := plugin.Resolve(root, ref)
		if err != nil {
			return fail(fmt.Errorf("plugin %q: %w", alias, err))
		}
		pin := store.RunPluginPin{Alias: alias, Name: ref.Name, Version: ref.Version, Digest: res.Digest}

		switch res.Manifest.Kind {
		case plugin.KindTool:
			if use.EffectiveLifetime() != declare.LifetimeRun {
				return fail(fmt.Errorf("plugin %q is a tool with lifetime %q; a tool execs per call and is always run-lifetime", alias, use.EffectiveLifetime()))
			}
			tools[alias] = res

		case plugin.KindService:
			if services == nil {
				return fail(fmt.Errorf("plugin %q is a service but no service supervisor is wired", alias))
			}
			var ses *serviceSession
			switch use.EffectiveLifetime() {
			case declare.LifetimeRun:
				ses, err = services.spawnRunInstance(res)
				if err == nil {
					runInstances = append(runInstances, ses)
				}
			case declare.LifetimeLane:
				ses, err = services.acquire("lane/"+scope.laneKey()+"/"+alias+"/"+ref.String(), res, use.Fresh)
			case declare.LifetimeResident:
				ses, err = services.acquire("resident/"+ref.String(), res, use.Fresh)
			default:
				err = fmt.Errorf("unknown lifetime %q", use.EffectiveLifetime())
			}
			if err != nil {
				return fail(fmt.Errorf("plugin %q: %w", alias, err))
			}
			pin.InstanceID = ses.id
			byAlias[alias] = &serviceCaller{session: ses, manifest: res.Manifest}

		default:
			return fail(fmt.Errorf("plugin %q is kind %q; a plugin is a tool or a service", alias, res.Manifest.Kind))
		}

		verbs := map[string]bool{}
		for verb := range res.Manifest.Verbs {
			verbs[verb] = true
		}
		rp.calls[alias] = verbs
		rp.pins = append(rp.pins, pin)
	}
	if len(tools) > 0 {
		tc := &toolCaller{runner: runner, resolved: tools, stderr: stderr}
		for alias := range tools {
			byAlias[alias] = tc
		}
	}
	rp.caller = &spillingCaller{inner: routingCaller(byAlias), dir: scope.SpillDir}
	if len(runInstances) > 0 {
		rp.cleanup = func() {
			for _, s := range runInstances {
				s.end()
			}
		}
	}
	return rp, nil
}

// payloadsDir is where spilled plugin payloads land under a workspace:
// content-addressed files a run's script reads by the reply's payload_path.
func payloadsDir(workspace string) string {
	return filepath.Join(workspace, ".iris", "payloads")
}

// routingCaller dispatches a call to its alias's caller. The collector has
// already enforced the declared surface, so a miss is an internal fault.
type routingCaller map[string]pluginCaller

// Call routes one call by alias.
func (r routingCaller) Call(ctx context.Context, call dispatch.TurnCall) (json.RawMessage, error) {
	c, ok := r[call.Alias]
	if !ok {
		return nil, fmt.Errorf("plugin %q is not resolved for this run", call.Alias)
	}
	return c.Call(ctx, call)
}

// maxPluginResultBytes bounds one call's inline result; a larger result is
// spilled to a payload file (or fails when no spill directory is configured).
const maxPluginResultBytes = 1 << 20

// maxSpillResultBytes is the hard ceiling on any result, spilled included.
const maxSpillResultBytes = 64 << 20

// spillingCaller wraps a caller with the oversized-result contract: a result
// past the inline cap is written to a content-addressed file under dir and the
// reply becomes {"payload_path":…,"sha256":…,"bytes":…}, so the run's ledger
// digest still names the full body while the run's stdin carries one line.
type spillingCaller struct {
	inner pluginCaller
	dir   string
}

// spilledPayload is the reply shape of a spilled result.
type spilledPayload struct {
	PayloadPath string `json:"payload_path"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
}

// Call services the inner call and spills an oversized result.
func (s *spillingCaller) Call(ctx context.Context, call dispatch.TurnCall) (json.RawMessage, error) {
	result, err := s.inner.Call(ctx, call)
	if err != nil || len(result) <= maxPluginResultBytes {
		return result, err
	}
	if len(result) > maxSpillResultBytes {
		return nil, fmt.Errorf("plugin %q verb %q result exceeds the %d-byte ceiling", call.Alias, call.Verb, maxSpillResultBytes)
	}
	if s.dir == "" {
		return nil, fmt.Errorf("plugin %q verb %q result exceeds %d bytes and no payload directory is configured", call.Alias, call.Verb, maxPluginResultBytes)
	}
	digest := plugin.Digest(result)
	path := filepath.Join(s.dir, digest+".json")
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, fmt.Errorf("plugin %q verb %q: payload dir: %w", call.Alias, call.Verb, err)
	}
	// Content-addressed: an existing file already holds these exact bytes.
	if _, statErr := os.Stat(path); statErr != nil {
		tmp, err := os.CreateTemp(s.dir, "."+digest+".tmp-*")
		if err != nil {
			return nil, fmt.Errorf("plugin %q verb %q: spill: %w", call.Alias, call.Verb, err)
		}
		if _, err := tmp.Write(result); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return nil, fmt.Errorf("plugin %q verb %q: spill: %w", call.Alias, call.Verb, err)
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmp.Name())
			return nil, fmt.Errorf("plugin %q verb %q: spill: %w", call.Alias, call.Verb, err)
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			_ = os.Remove(tmp.Name())
			return nil, fmt.Errorf("plugin %q verb %q: spill: %w", call.Alias, call.Verb, err)
		}
	}
	reply, err := json.Marshal(spilledPayload{PayloadPath: path, SHA256: digest, Bytes: int64(len(result))})
	if err != nil {
		return nil, fmt.Errorf("plugin %q verb %q: encode spill reply: %w", call.Alias, call.Verb, err)
	}
	return reply, nil
}

// serviceCaller relays a call to a supervised service instance with the
// manifest's per-verb timeout.
type serviceCaller struct {
	session  *serviceSession
	manifest plugin.Manifest
}

// Call relays one verb call to the instance.
func (c *serviceCaller) Call(ctx context.Context, call dispatch.TurnCall) (json.RawMessage, error) {
	timeout := time.Duration(c.manifest.Verbs[call.Verb].Timeout()) * time.Second
	return c.session.call(ctx, call.Verb, call.Args, timeout)
}

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
	if len(raw) > maxSpillResultBytes {
		return nil, fmt.Errorf("plugin %q verb %q result exceeds the %d-byte ceiling", call.Alias, call.Verb, maxSpillResultBytes)
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
