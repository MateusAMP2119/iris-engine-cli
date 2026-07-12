package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// The pinned tour strings the flow tests assert. They are the operator-facing
// contract surface of the sequencer: the two prompt questions, the resume hint
// every abort and failure prints, and the exit-5 dead-letter lesson.
const (
	wantCommandPrompt   = "Run it? (Y/n)"
	wantWorkspacePrompt = "Run the tour in this workspace? (Y/n)"
	wantCreatePrompt    = "Create ./iris-quickstart-demo and work there? (Y/n)"
	wantResumeHint      = "Resume any time: iris quickstart"
	wantDeadletterShow  = "iris deadletter show hello_iris"
)

// tourApp builds an app for driving `iris quickstart` with both TTY seams
// forced to tty.
func tourApp(out, errb *bytes.Buffer, tty bool) *app {
	a := newApp(out, errb)
	a.isTTY = func() bool { return tty }
	a.stdinIsTTY = func() bool { return tty }
	return a
}

// scriptTour installs a scripted tourPrompt and a recording runStep on a. The
// prompt consumes answers in order (quitting when they run out) and the runStep
// fake returns the code of the first matching argv prefix in codes (0 with no
// match). Both append to the returned event log -- "prompt <question>" and
// "step <argv...>" -- so a test can assert the explain-confirm-execute order.
func scriptTour(a *app, answers []promptAnswer, codes map[string]int) *[]string {
	events := &[]string{}
	next := 0
	a.tourPrompt = func(question string, _ promptKind) (promptAnswer, error) {
		*events = append(*events, "prompt "+question)
		if next >= len(answers) {
			return answerQuit, nil
		}
		ans := answers[next]
		next++
		return ans, nil
	}
	a.runStep = func(_ context.Context, args []string) int {
		joined := strings.Join(args, " ")
		*events = append(*events, "step "+joined)
		for prefix, code := range codes {
			if strings.HasPrefix(joined, prefix) {
				return code
			}
		}
		return 0
	}
	return events
}

// proceeds returns n affirmative answers.
func proceeds(n int) []promptAnswer {
	out := make([]promptAnswer, n)
	for i := range out {
		out[i] = answerProceed
	}
	return out
}

// stepEvents filters the executed-step entries out of a tour event log,
// stripping the "step " tag.
func stepEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if rest, ok := strings.CutPrefix(e, "step "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// promptEvents filters the asked-question entries out of a tour event log,
// stripping the "prompt " tag.
func promptEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if rest, ok := strings.CutPrefix(e, "prompt "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// canonicalStepArgvs returns the six tour commands as the argv each one hands
// the in-process runner: the canonical table rows with the literal "iris"
// argv[0] stripped (in-process re-entry, never a binary path or PATH lookup).
func canonicalStepArgvs() []string {
	var out []string
	for _, s := range quickstartSteps() {
		out = append(out, strings.Join(s.Argv[1:], " "))
	}
	return out
}

// chdirWorkspace moves the test into a fresh temp directory that already is a
// workspace (a pipelines/ folder exists), so the tour's workspace step proposes
// using it rather than creating ./iris-quickstart-demo.
func chdirWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "pipelines"), 0o755); err != nil {
		t.Fatalf("create pipelines dir: %v", err)
	}
	t.Chdir(dir)
	return dir
}

// startHealthzDaemon serves the real api mux over a unix socket -- a bare mux,
// whose GET /healthz answers ok on every role -- the integration-tier
// reachable-daemon pattern (in-process daemon over a socket, no live Postgres).
func startHealthzDaemon(t *testing.T, sock string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: api.NewMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// TestQuickstartRefusesRemoteHost proves the tour refuses --host as a usage
// error (exit 2) with local-tour guidance -- the tour provisions a local engine
// -- while --socket stays accepted, in every rendering mode.
func TestQuickstartRefusesRemoteHost(t *testing.T) {
	// spec: S08/quickstart-refuses-remote-host
	t.Run("S08/quickstart-refuses-remote-host", func(t *testing.T) {
		t.Run("--host is a usage error with local-tour guidance", func(t *testing.T) {
			for _, tty := range []bool{false, true} {
				var out, errb bytes.Buffer
				a := tourApp(&out, &errb, tty)
				events := scriptTour(a, nil, nil)
				code := a.run([]string{"quickstart", "--host", "10.0.0.5:7433"})
				if code != exitUsage {
					t.Fatalf("tty=%v: exit = %d, want %d\nstdout: %s\nstderr: %s", tty, code, exitUsage, out.String(), errb.String())
				}
				if msg := errb.String(); !strings.Contains(msg, "--host") || !strings.Contains(msg, "local") {
					t.Errorf("tty=%v: refusal does not carry local-tour guidance naming --host: %q", tty, msg)
				}
				if len(*events) != 0 {
					t.Errorf("tty=%v: refused quickstart still prompted or executed: %v", tty, *events)
				}
			}
		})
		t.Run("--host under --json is the usage error envelope", func(t *testing.T) {
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, nil, nil)
			code := a.run([]string{"quickstart", "--json", "--host", "10.0.0.5:7433"})
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d", code, exitUsage)
			}
			if !looksJSON(out.Bytes()) {
				t.Errorf("--json refusal did not render the error envelope on stdout: %q", out.String())
			}
			if len(*events) != 0 {
				t.Errorf("refused quickstart still prompted or executed: %v", *events)
			}
		})
		t.Run("--socket stays accepted", func(t *testing.T) {
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			code := a.run([]string{"quickstart", "--socket", "/tmp/iris-tour.sock"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (a local --socket must stay accepted)\nstderr: %s", code, exitOK, errb.String())
			}
		})
	})
}

// TestQuickstartStepOrderConfirmed proves the interactive sequencer: the six
// canonical steps execute in tour order, each one only after its own
// affirmative prompt, handed to the in-process runner as the canonical argv
// with the literal "iris" argv[0] stripped -- never a binary path or PATH
// lookup.
func TestQuickstartStepOrderConfirmed(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-step-order-confirmed
	t.Run("S08/quickstart-step-order-confirmed", func(t *testing.T) {
		t.Run("each step runs in order, only after its prompt", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(7), nil) // workspace + six steps

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}

			want := []string{"prompt " + wantWorkspacePrompt}
			for _, argv := range canonicalStepArgvs() {
				want = append(want, "prompt "+wantCommandPrompt, "step "+argv)
			}
			if got := *events; !equalStrings(got, want) {
				t.Errorf("tour event order:\n got %q\nwant %q", got, want)
			}

			// The tour shows each literal command before asking.
			for _, s := range quickstartSteps() {
				if cmdLine := "$ " + strings.Join(s.Argv, " "); !strings.Contains(out.String(), cmdLine) {
					t.Errorf("tour never showed the literal command %q\nstdout: %s", cmdLine, out.String())
				}
			}
			// The wrap-up leaves the engine running and says so.
			if !strings.Contains(out.String(), "still running") {
				t.Errorf("wrap-up does not note the engine is left running\nstdout: %s", out.String())
			}
		})

		t.Run("skipping one step keeps the rest in order", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			// Skip step 3 (info); everything else proceeds.
			answers := []promptAnswer{answerProceed, answerProceed, answerProceed, answerSkip, answerProceed, answerProceed, answerProceed}
			events := scriptTour(a, answers, nil)

			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			all := canonicalStepArgvs()
			want := append(append([]string{}, all[:2]...), all[3:]...)
			if got := stepEvents(*events); !equalStrings(got, want) {
				t.Errorf("steps with info skipped:\n got %q\nwant %q", got, want)
			}
		})
	})
}

// TestQuickstartDeclineCleanAbort proves declining any prompt -- or EOF, or an
// interrupt -- stops the tour cleanly: exit 0, a resume hint, and nothing past
// the decline executed.
func TestQuickstartDeclineCleanAbort(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-decline-clean-abort
	t.Run("S08/quickstart-decline-clean-abort", func(t *testing.T) {
		t.Run("declining prompt k executes nothing past it", func(t *testing.T) {
			// Prompt 0 is the workspace question; prompt k>=1 precedes step k, so a
			// decline at prompt k leaves exactly steps 1..k-1 executed.
			for declineAt := 0; declineAt <= 6; declineAt++ {
				chdirWorkspace(t)
				var out, errb bytes.Buffer
				a := tourApp(&out, &errb, true)
				events := scriptTour(a, proceeds(declineAt), nil) // quit once answers run out

				code := a.run([]string{"quickstart"})
				if code != exitOK {
					t.Fatalf("decline at prompt %d: exit = %d, want %d (a decline is never a failure)\nstderr: %s",
						declineAt, code, exitOK, errb.String())
				}
				if !strings.Contains(out.String(), wantResumeHint) {
					t.Errorf("decline at prompt %d: no resume hint %q\nstdout: %s", declineAt, wantResumeHint, out.String())
				}
				var want []string
				if declineAt >= 1 {
					want = canonicalStepArgvs()[:declineAt-1]
				}
				if got := stepEvents(*events); !equalStrings(got, want) {
					t.Errorf("decline at prompt %d executed:\n got %q\nwant %q", declineAt, got, want)
				}
			}
		})

		t.Run("EOF on a prompt aborts clean", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			a.tourPrompt = func(string, promptKind) (promptAnswer, error) { return answerQuit, io.EOF }
			a.runStep = func(context.Context, []string) int {
				t.Error("a step executed after EOF")
				return 0
			}
			code := a.run([]string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (EOF is a clean abort)\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), wantResumeHint) {
				t.Errorf("EOF abort carries no resume hint\nstdout: %s", out.String())
			}
		})

		t.Run("interrupt while a prompt is open aborts clean", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			release := make(chan struct{})
			var reachedOnce sync.Once
			reached := make(chan struct{})
			a.tourPrompt = func(string, promptKind) (promptAnswer, error) {
				reachedOnce.Do(func() { close(reached) })
				<-release // held open until the test ends: the interrupt must win
				return answerQuit, nil
			}
			a.runStep = func(context.Context, []string) int {
				t.Error("a step executed after the interrupt")
				return 0
			}
			t.Cleanup(func() { close(release) })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-reached
				cancel() // the Ctrl-C path: the tour's signal-bound context cancels
			}()

			code := a.runContext(ctx, []string{"quickstart"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (an interrupt is a clean abort)\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), wantResumeHint) {
				t.Errorf("interrupt abort carries no resume hint\nstdout: %s", out.String())
			}
		})
	})
}

// TestQuickstartAdaptiveSkipRunningEngine proves the adaptive probe: with a
// daemon answering /healthz on the workspace socket, the tour announces
// install and start as already done, skips them without prompting, and
// proceeds from the info step -- every remaining step targeting that socket.
func TestQuickstartAdaptiveSkipRunningEngine(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-adaptive-skip-running-engine
	t.Run("S08/quickstart-adaptive-skip-running-engine", func(t *testing.T) {
		chdirWorkspace(t)
		sock := shortSocket(t)
		startHealthzDaemon(t, sock)

		var out, errb bytes.Buffer
		a := tourApp(&out, &errb, true)
		events := scriptTour(a, proceeds(5), nil) // workspace + the four remaining steps

		code := a.run([]string{"quickstart", "--socket", sock})
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
		}

		steps := stepEvents(*events)
		if len(steps) != 4 {
			t.Fatalf("tour executed %d steps %q, want the 4 past install/start", len(steps), steps)
		}
		wantPrefixes := []string{"engine info", "declare apply pipelines/hello_iris", "pipeline run hello_iris", "data provenance demo.colors green"}
		for i, prefix := range wantPrefixes {
			if !strings.HasPrefix(steps[i], prefix) {
				t.Errorf("step[%d] = %q, want prefix %q (tour proceeds from the info step)", i, steps[i], prefix)
			}
			if !strings.Contains(steps[i], "--socket="+sock) {
				t.Errorf("step[%d] = %q does not target the tour's --socket", i, steps[i])
			}
		}
		for _, s := range steps {
			if strings.HasPrefix(s, "engine install") || strings.HasPrefix(s, "engine start") {
				t.Errorf("already-done step still executed: %q", s)
			}
		}
		if prompts := promptEvents(*events); len(prompts) != 5 {
			t.Errorf("tour asked %d prompts %q, want 5 (skipped steps are announced, never prompted)", len(prompts), prompts)
		}
		if !strings.Contains(out.String(), "already") {
			t.Errorf("tour does not announce install/start as already done\nstdout: %s", out.String())
		}
	})
}

// TestQuickstartYesRunsUnattended proves --yes: every step runs without a
// single prompt, piped output carries no ANSI escape, the workspace is created
// unattended, and a failing step's exit category becomes the tour's exit code.
func TestQuickstartYesRunsUnattended(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-yes-runs-unattended
	t.Run("S08/quickstart-yes-runs-unattended", func(t *testing.T) {
		t.Run("piped --yes runs every step with zero prompts and zero ANSI", func(t *testing.T) {
			dir := t.TempDir() // empty: not a workspace, so --yes must create the demo dir
			t.Chdir(dir)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false) // piped: no TTY anywhere
			events := scriptTour(a, nil, nil)

			code := a.run([]string{"quickstart", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}
			if prompts := promptEvents(*events); len(prompts) != 0 {
				t.Errorf("--yes still prompted: %q", prompts)
			}
			if got := stepEvents(*events); !equalStrings(got, canonicalStepArgvs()) {
				t.Errorf("--yes steps:\n got %q\nwant %q", got, canonicalStepArgvs())
			}
			assertNoEsc(t, out.String())
			assertNoEsc(t, errb.String())

			// The workspace step ran unattended: the demo dir exists and the sample
			// was materialized into it before the apply step.
			decl := filepath.Join(dir, "iris-quickstart-demo", "pipelines", "hello_iris", "iris-declare.yaml")
			if _, err := os.Stat(decl); err != nil {
				t.Errorf("--yes did not create the demo workspace with the sample: %v", err)
			}
		})

		t.Run("first failing step's category is the exit code", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			events := scriptTour(a, nil, map[string]int{"pipeline run": 5})

			code := a.run([]string{"quickstart", "--yes"})
			if code != exitDeadLettered {
				t.Fatalf("exit = %d, want %d (the failing step's own category)\nstderr: %s", code, exitDeadLettered, errb.String())
			}
			if got, want := stepEvents(*events), canonicalStepArgvs()[:5]; !equalStrings(got, want) {
				t.Errorf("steps past the failure executed:\n got %q\nwant %q", got, want)
			}
			low := strings.ToLower(errb.String())
			if !strings.Contains(low, strings.ToLower(wantResumeHint)) {
				t.Errorf("failure carries no resume hint on stderr: %q", errb.String())
			}
			if !strings.Contains(errb.String(), wantDeadletterShow) {
				t.Errorf("exit-5 failure does not teach the dead-letter lesson (%q): %q", wantDeadletterShow, errb.String())
			}
		})

		t.Run("an early failure stops before later steps", func(t *testing.T) {
			chdirWorkspace(t)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			events := scriptTour(a, nil, map[string]int{"engine install": 4})

			code := a.run([]string{"quickstart", "--yes"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOpFailed, errb.String())
			}
			if got, want := stepEvents(*events), canonicalStepArgvs()[:1]; !equalStrings(got, want) {
				t.Errorf("steps past the install failure executed:\n got %q\nwant %q", got, want)
			}
		})

		t.Run("--json beats --yes: the envelope, executing nothing", func(t *testing.T) {
			scratch := t.TempDir()
			t.Chdir(scratch)
			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, false)
			events := scriptTour(a, nil, nil)

			code := a.run([]string{"quickstart", "--yes", "--json"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if !looksJSON(out.Bytes()) {
				t.Errorf("--json --yes did not render the envelope: %q", out.String())
			}
			if len(*events) != 0 {
				t.Errorf("--json --yes prompted or executed: %v", *events)
			}
			requireEmptyDir(t, scratch)
		})
	})
}

// equalStrings reports whether two string slices are equal element-wise.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestQuickstartIgnoresAmbientHost proves the tour's local-only targeting past
// the flag refusal: with an ambient IRIS_HOST configured (a resolution input
// the --host flag refusal cannot see), every daemon-touching step still dials
// the LOCAL workspace daemon over its socket -- the steps run through the real
// in-process child runner, real HTTP -- while a stand-in "remote" listener at
// the ambient host receives zero requests, and the tour announces once that
// the configured host is ignored.
func TestQuickstartIgnoresAmbientHost(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-refuses-remote-host
	t.Run("S08/quickstart-refuses-remote-host", func(t *testing.T) {
		t.Run("ambient IRIS_HOST never reaches a tour step", func(t *testing.T) {
			chdirWorkspace(t)

			// The local workspace daemon: the real mux over a unix socket, recording
			// every request path it serves.
			sock := shortSocket(t)
			var mu sync.Mutex
			var localPaths []string
			mux := api.NewMux()
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatalf("listen unix %s: %v", sock, err)
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				localPaths = append(localPaths, r.URL.Path)
				mu.Unlock()
				mux.ServeHTTP(w, r)
			}), ReadHeaderTimeout: 5 * time.Second}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

			// The stand-in remote at the ambient IRIS_HOST: it must see zero requests.
			var remoteHits atomic.Int64
			rln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen tcp: %v", err)
			}
			rsrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				remoteHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}), ReadHeaderTimeout: 5 * time.Second}
			go func() { _ = rsrv.Serve(rln) }()
			t.Cleanup(func() { _ = rsrv.Shutdown(context.Background()) })
			remoteAddr := rln.Addr().String()
			t.Setenv("IRIS_HOST", remoteAddr)

			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			key, kerr := daemon.MintEngineKey()
			if kerr != nil {
				t.Fatalf("MintEngineKey: %v", kerr)
			}
			a.newKeyReader = func(config.Settings) daemon.EngineKeyReader { return fakeKeyReader{key: key} }
			// Scripted prompt only: the steps run through the REAL in-process child
			// runner. The bare mux answers apply/run/provenance with error envelopes,
			// so the wrapper swallows exit codes to walk every step -- this test
			// asserts dial targets, not step outcomes. install/start must never
			// execute for real (the reachable workspace daemon skips them); reaching
			// them is itself a failure.
			a.tourPrompt = func(string, promptKind) (promptAnswer, error) { return answerProceed, nil }
			a.runStep = func(ctx context.Context, args []string) int {
				joined := strings.Join(args, " ")
				if strings.HasPrefix(joined, "engine install") || strings.HasPrefix(joined, "engine start") {
					t.Errorf("install/start executed despite the reachable workspace daemon: %q", joined)
					return 0
				}
				_ = a.runTourChild(ctx, args)
				return 0
			}

			code := a.run([]string{"quickstart", "--socket", sock})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}

			if got := remoteHits.Load(); got != 0 {
				t.Errorf("the ambient IRIS_HOST %s received %d requests, want 0 (the tour must never target a remote engine)", remoteAddr, got)
			}
			mu.Lock()
			paths := append([]string(nil), localPaths...)
			mu.Unlock()
			for _, wantPrefix := range []string{"/info", "/apply", "/pipeline/run", "/provenance/"} {
				if !hasPathWithPrefix(paths, wantPrefix) {
					t.Errorf("the local workspace daemon never received %s* (served paths: %q); that step dialed elsewhere", wantPrefix, paths)
				}
			}
			if !strings.Contains(out.String(), remoteAddr) || !strings.Contains(out.String(), "local") {
				t.Errorf("tour does not announce the ignored ambient host %s\nstdout: %s", remoteAddr, out.String())
			}
		})
	})
}

// hasPathWithPrefix reports whether any served path starts with prefix.
func hasPathWithPrefix(paths []string, prefix string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
