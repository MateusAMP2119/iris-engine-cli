package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// crossModeReaderYAML is a pipeline declaration that reads one upstream table.
// The seam under test treats the reader as permanent-data and its upstream as
// disposable-mode, the mid-promotion state that raises a cross-mode read warning.
const crossModeReaderYAML = `name: load_orders
run: [python, main.py]
reads:
  - table: raw.orders_staging
    fields: [id]
`

// applyEnvelope is the shape apply's --json output takes: the terminal envelope
// (an error envelope here, since no daemon is reachable yet) with any advisory
// warnings riding alongside as a first-class array.
type applyEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Warnings []declare.Warning `json:"warnings"`
}

// TestCrossModeWarningRidesJSON proves the cross-mode read warning is carried in
// apply's --json output (specification section 5), and that the warning
// accompanies the apply rather than replacing it: apply still falls through to the
// daemon dial (exit 3, no daemon reachable), and the warning rides the single
// terminal --json envelope. The applyWarnings seam stands in for the meta-backed
// data-mode facts apply reads once it runs against the daemon (E03.9/E03.10); here
// it runs the real declare.CheckCrossModeReads over the parsed declaration, so what
// is proven is the warning structure riding the --json envelope end to end.
func TestCrossModeWarningRidesJSON(t *testing.T) {
	// spec: S05/cross-mode-warning-rides-json
	dir := t.TempDir()
	target := filepath.Join(dir, "iris-declare.yaml")
	if err := os.WriteFile(target, []byte(crossModeReaderYAML), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sock := shortSocket(t)
	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	// Inject the meta-backed data-mode facts: this reader is permanent-data, and
	// every table it reads is a disposable-mode upstream (mid-promotion).
	a.applyWarnings = func(d *declare.Declaration) []declare.Warning {
		if d.Kind != declare.KindPipeline {
			return nil
		}
		var ups []declare.UpstreamRead
		for _, r := range d.Pipeline.Reads {
			ups = append(ups, declare.UpstreamRead{Table: r.Table, Mode: declare.DataDisposable})
		}
		return declare.CheckCrossModeReads(declare.DataPermanent, ups)
	}

	code := a.run([]string{"--socket", sock, "--json", "declare", "apply", target})
	// The warning accompanies apply; it never blocks it. Apply still dials the
	// daemon and reports no-daemon (exit 3) -- proof the warning did not short-circuit
	// the actual apply.
	if code != exitNoDaemon {
		t.Fatalf("exit = %d, want %d (warning accompanies apply, never replaces the daemon dial)\nstdout: %s\nstderr: %s", code, exitNoDaemon, out.String(), errb.String())
	}

	// stdout is exactly one JSON document -- the terminal envelope carrying warnings.
	var env applyEnvelope
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\nstdout: %q", err, out.String())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries content after the single JSON envelope: %q", out.String())
	}

	// The terminal outcome is still the no-daemon error, proving apply proceeded.
	if env.Error.Message == "" {
		t.Errorf("terminal envelope has no error; apply must still report the daemon dial\nstdout: %s", out.String())
	}
	if len(env.Warnings) != 1 {
		t.Fatalf("envelope warnings = %d, want 1\nstdout: %s", len(env.Warnings), out.String())
	}
	w := env.Warnings[0]
	if w.Kind != declare.WarnCrossModeRead {
		t.Errorf("warning kind = %q, want cross_mode_read", w.Kind)
	}
	if w.Table != "raw.orders_staging" {
		t.Errorf("warning table = %q, want raw.orders_staging", w.Table)
	}
	if w.Message == "" {
		t.Error("warning rode --json with no message")
	}
}

// TestApplyNoWarningsUnchanged proves the warning seam never disturbs the ordinary
// apply path: with no cross-mode warning to surface, apply resolves its target and
// reaches the daemon-dial stub exactly as before (exit 3, no daemon reachable),
// and its --json envelope carries no warnings array. This pins that the --json
// warning surface is additive, not a behavior change to apply's existing single-
// file resolution contract.
func TestApplyNoWarningsUnchanged(t *testing.T) {
	// spec: S05/cross-mode-warning-rides-json
	dir := t.TempDir()
	target := filepath.Join(dir, "iris-declare.yaml")
	if err := os.WriteFile(target, []byte(crossModeReaderYAML), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sock := shortSocket(t)
	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	// A reader with no disposable upstream produces no warning; apply proceeds.
	a.applyWarnings = func(_ *declare.Declaration) []declare.Warning {
		return declare.CheckCrossModeReads(declare.DataPermanent, nil)
	}
	code := a.run([]string{"--socket", sock, "--json", "declare", "apply", target})
	if code != exitNoDaemon {
		t.Fatalf("exit = %d, want %d (no warning: apply reaches the daemon-dial stub)\nstdout: %s\nstderr: %s", code, exitNoDaemon, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "warnings") {
		t.Errorf("no-warning apply emitted a warnings field: %q", out.String())
	}
}
