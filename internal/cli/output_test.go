package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestLogsSeparateFromCommandOutput proves that the CLI's structured log output
// goes only to stderr and is never interleaved into a command's stdout stream:
// under --json, stdout carries exactly the single error envelope while the
// daemon-reachability log line lands on stderr. A debug-level logger forces the
// otherwise-quiet diagnostic out so the test can prove where it lands.
func TestLogsSeparateFromCommandOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := newAppWithLogger(&stdout, &stderr, logger)

	code := a.run([]string{"--json", "pipeline", "list"})
	if code != exitNoDaemon {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitNoDaemon, stdout.String(), stderr.String())
	}

	// stdout is exactly one JSON document -- the error envelope -- and nothing else.
	var env errEnvelope
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\nstdout: %q", err, stdout.String())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries content after the single JSON envelope: %q", stdout.String())
	}
	if env.Error.Message == "" {
		t.Errorf("envelope has no message: %+v", env)
	}

	// No log line leaked into stdout.
	if strings.Contains(stdout.String(), "level=") || strings.Contains(stdout.String(), "no iris daemon") {
		t.Errorf("a log line leaked into stdout: %q", stdout.String())
	}

	// The daemon-reachability log line is on stderr, separate from command output.
	if !strings.Contains(stderr.String(), "no iris daemon reachable") {
		t.Errorf("the daemon-reachability log line is missing from stderr: %q", stderr.String())
	}
}
