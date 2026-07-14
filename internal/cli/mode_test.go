package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// decodeSingleJSON asserts b is exactly one JSON document and decodes it into v.
func decodeSingleJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\nstdout: %q", err, b)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries content after the single JSON document: %q", b)
	}
}

// looksJSON reports whether stdout was emitted as a JSON document (the CLI's
// envelopes are objects), i.e. the command ran in JSON mode.
func looksJSON(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && t[0] == '{'
}

// TestJSONNeverLeavesNonJSONOnStdout proves the --json contract holds for the
// tree's group nodes and the root, which must never print human help to stdout
// under --json: a bare noun or sub-noun under --json is a single JSON error
// envelope (exit 2), and the bare root under --json is a single JSON data
// document (exit 0). Human bare root stays help/exit 0.
func TestJSONNeverLeavesNonJSONOnStdout(t *testing.T) {
	groups := [][]string{
		{"declare"}, {"pipeline"}, {"run"}, {"data"}, {"workload"},
		{"engine"}, {"engine", "service"}, {"deadletter"}, {"endpoint"}, {"pat"},
	}
	for _, g := range groups {
		t.Run("bare "+strings.Join(g, " "), func(t *testing.T) {
			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run(append([]string{"--json"}, g...))
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d (usage)\nstdout: %q", code, exitUsage, out.String())
			}
			var env errEnvelope
			decodeSingleJSON(t, out.Bytes(), &env)
			if env.Error.Code == "" || env.Error.Message == "" {
				t.Errorf("group envelope missing code/message: %+v", env)
			}
		})
	}

	t.Run("bare root --json is one data document", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--json"})
		if code != exitOK {
			t.Fatalf("bare root --json: exit = %d, want %d\nstdout: %q", code, exitOK, out.String())
		}
		// Exactly one JSON document, decoded structurally (not coupled to the
		// internal envelope type), with a "data" object.
		var doc struct {
			Data map[string]any `json:"data"`
		}
		decodeSingleJSON(t, out.Bytes(), &doc)
		if doc.Data == nil {
			t.Errorf("bare root --json document has no data object: %q", out.String())
		}
	})

	t.Run("bare root human stays help exit 0", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run(nil)
		if code != exitOK {
			t.Fatalf("bare root: exit = %d, want %d\nstdout: %q", code, exitOK, out.String())
		}
		if out.Len() == 0 {
			t.Error("bare root printed no help to stdout")
		}
	})
}

// TestJSONModeMatchesPflagConsumption proves the output mode honors exactly how
// pflag consumed --json: a --json swallowed as the value of a value-taking flag
// (global --token, or the per-command --after) is not JSON mode, so stdout stays
// clean and the error is human on stderr; a real --json is JSON mode, including
// when it follows a flag-parse error and is resolved by the pflag probe.
func TestJSONModeMatchesPflagConsumption(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantJSON bool
	}{
		{"leading --json flag", []string{"--json", "pipeline", "list"}, true},
		{"--json interspersed after positionals", []string{"pipeline", "list", "--json"}, true},
		{"--json swallowed by global --token", []string{"--token", "--json", "pipeline", "list"}, false},
		{"--json swallowed by per-command --after", []string{"run", "list", "--after", "--json"}, false},
		{"--json after a bad flag resolves via probe", []string{"pipeline", "list", "--bogus", "--json"}, true},
		// The probe must honor per-command value flags too: here --after swallows
		// --json in the real parse and a later --bogus makes it the flag-error path,
		// so the probe (not the parsed flag) resolves the mode -- and must still see
		// --json as consumed, not set.
		{"--json swallowed by --after even when a later flag errors", []string{"run", "list", "--after", "--json", "--bogus"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			newApp(&out, &errb).run(tc.args)
			if got := looksJSON(out.Bytes()); got != tc.wantJSON {
				t.Fatalf("stdout is JSON = %v, want %v\nstdout: %q\nstderr: %q",
					got, tc.wantJSON, out.String(), errb.String())
			}
			if tc.wantJSON {
				var env errEnvelope
				decodeSingleJSON(t, out.Bytes(), &env)
			} else if strings.TrimSpace(out.String()) != "" {
				t.Errorf("human mode left non-JSON content on stdout: %q", out.String())
			}
		})
	}
}
