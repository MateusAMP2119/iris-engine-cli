package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCeremonyLogRecordsLinesWithoutDupOnNote(t *testing.T) {
	var out bytes.Buffer
	log := newCeremonyLog(&out)
	log.line("  hello")
	log.note("  noted")
	if out.String() != "  hello\n" {
		t.Fatalf("out = %q, want only printed lines", out.String())
	}
	got := log.content()
	if !strings.Contains(got, "  hello") || !strings.Contains(got, "  noted") {
		t.Fatalf("content = %q, want both lines", got)
	}
}

func TestAppendCeremonyLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ceremony.log")
	t.Setenv(ceremonyLogPathEnv, path)
	appendCeremonyLogFile("line-one")
	appendCeremonyLogFile("line-two")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "line-one\nline-two\n" {
		t.Fatalf("file = %q", b)
	}
}

func TestCeremonyReviewDisabled(t *testing.T) {
	t.Setenv(ceremonyNoReviewEnv, "")
	t.Setenv(ceremonyLogPathEnv, "")
	t.Setenv(ceremonyReviewEnv, "")
	if !ceremonyReviewDisabled() {
		t.Fatal("expected disabled by default (no blocking pager)")
	}
	t.Setenv(ceremonyReviewEnv, "1")
	if ceremonyReviewDisabled() {
		t.Fatal("expected enabled when IRIS_CEREMONY_REVIEW=1")
	}
	t.Setenv(ceremonyNoReviewEnv, "1")
	if !ceremonyReviewDisabled() {
		t.Fatal("expected disabled with IRIS_NO_CEREMONY_REVIEW")
	}
	t.Setenv(ceremonyNoReviewEnv, "")
	t.Setenv(ceremonyLogPathEnv, "/tmp/x")
	if !ceremonyReviewDisabled() {
		t.Fatal("expected disabled when parent owns IRIS_CEREMONY_LOG")
	}
}

func TestMaybeReviewCeremonyNoTTY(t *testing.T) {
	t.Setenv(ceremonyNoReviewEnv, "")
	t.Setenv(ceremonyLogPathEnv, "")
	var out bytes.Buffer
	// Non-TTY writer: must not panic and must not write pager chrome.
	long := strings.Repeat("line\n", 100)
	maybeReviewCeremony(&out, long)
	if out.Len() != 0 {
		t.Fatalf("pager wrote to non-TTY: %q", out.String())
	}
}

func TestProgressFinalLineSettled(t *testing.T) {
	line := progressFinalLine("• Removing engine state")
	if !strings.Contains(line, "Removing engine state") {
		t.Fatalf("line missing label: %q", line)
	}
	if !strings.Contains(line, "100%") {
		t.Fatalf("line missing 100%%: %q", line)
	}
}

func TestReadCeremonyReviewContentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.log")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readCeremonyReviewContent([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\nb\n" {
		t.Fatalf("got %q", got)
	}
}

func TestTruncateDisplay(t *testing.T) {
	if got := truncateDisplay("hello", 10); got != "hello" {
		t.Fatalf("short = %q", got)
	}
	got := truncateDisplay("abcdefghij", 5)
	if lipglossWidth(got) > 5 {
		t.Fatalf("truncated width too large: %q", got)
	}
}

// lipglossWidth avoids importing lipgloss in every assertion helper name clash.
func lipglossWidth(s string) int {
	return len([]rune(s)) // help text is ASCII; good enough for this unit check
}
