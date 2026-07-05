package trace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
)

// SpecLock is the recorded fingerprint of the specification inventory, the
// source of truth. The gate recomputes the doc's fingerprint and compares it to
// SHA256: a mismatch means the spec changed since the lock was last recorded. An
// unrecorded change fails the gate (a spec delta without a test delta); the
// explicit update path re-records the lock alongside the accompanying test delta,
// which is how a behavioral spec change is admitted.
type SpecLock struct {
	SpecPath string `yaml:"spec_path"`
	SHA256   string `yaml:"sha256"`
}

// SpecDeltaError reports that the spec doc's fingerprint drifted from the lock.
type SpecDeltaError struct {
	Path string
	Want string // fingerprint recorded in the lock
	Got  string // fingerprint of the current doc
}

// Error describes the drift and points at the re-lock update path.
func (e *SpecDeltaError) Error() string {
	return fmt.Sprintf("trace: spec delta in %s without a test delta: locked %s, doc now %s (re-record spec/inventory.lock after amending the tests)", e.Path, short(e.Want), short(e.Got))
}

// Fingerprint returns the SHA-256 hex digest of content with CR characters
// stripped, so the fingerprint is stable across line-ending churn but changes
// with any real content edit.
func Fingerprint(content []byte) string {
	normalized := bytes.ReplaceAll(content, []byte("\r"), nil)
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:])
}

// Verify reports whether the spec doc content still matches the locked
// fingerprint, returning a *SpecDeltaError on drift and nil when they agree.
func (l SpecLock) Verify(content []byte) error {
	got := Fingerprint(content)
	if got != l.SHA256 {
		return &SpecDeltaError{Path: l.SpecPath, Want: l.SHA256, Got: got}
	}
	return nil
}

// LoadSpecLock reads and parses the spec lock at path.
func LoadSpecLock(path string) (SpecLock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SpecLock{}, fmt.Errorf("trace: read spec lock %s: %w", path, err)
	}
	var l SpecLock
	if err := yaml.Unmarshal(data, &l); err != nil {
		return SpecLock{}, fmt.Errorf("trace: parse spec lock %s: %w", path, err)
	}
	if l.SHA256 == "" {
		return SpecLock{}, fmt.Errorf("trace: spec lock %s carries no sha256", path)
	}
	return l, nil
}

// Write re-records the lock at path (the explicit spec-delta update path).
func (l SpecLock) Write(path string) error {
	var buf bytes.Buffer
	buf.WriteString("# spec/inventory.lock -- fingerprint of the spec inventory (the source of\n")
	buf.WriteString("# truth). The traceability gate fails when the doc's SHA-256 drifts from this\n")
	buf.WriteString("# value: a spec delta without a test delta. Re-record it (IRIS_TRACE_UPDATE_LOCK=1\n")
	buf.WriteString("# go test ./internal/trace -run TestSpecLockUpdate) only alongside the test delta.\n")
	data, err := yaml.Marshal(l)
	if err != nil {
		return fmt.Errorf("trace: marshal spec lock: %w", err)
	}
	buf.Write(data)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("trace: write spec lock %s: %w", path, err)
	}
	return nil
}

// short trims a hex digest for human-readable error messages.
func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
