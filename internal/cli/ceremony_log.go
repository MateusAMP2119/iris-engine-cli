package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ceremonyLogPathEnv is set by install.sh so shell and binary ceremony helpers
// share one transcript file for the post-install scrollback review.
const ceremonyLogPathEnv = "IRIS_CEREMONY_LOG"

// ceremonyNoReviewEnv disables the interactive scrollback viewer when set
// (legacy opt-out; auto-review is already off unless ceremonyReviewEnv is set).
const ceremonyNoReviewEnv = "IRIS_NO_CEREMONY_REVIEW"

// ceremonyReviewEnv must be set (non-empty) for automatic post-ceremony
// scrollback. Default install/uninstall/setup never block on a pager so
// shell chains like `install && iris uninstall` need no keypresses.
const ceremonyReviewEnv = "IRIS_CEREMONY_REVIEW"

// ceremonyLog records ceremony lines for post-run scrollback while still
// writing live output to the terminal. Progress-bar animation stays on out;
// only the final settled line is noted (via note), so the transcript stays clean.
type ceremonyLog struct {
	out   io.Writer
	lines []string
}

func newCeremonyLog(out io.Writer) *ceremonyLog {
	if out == nil {
		out = io.Discard
	}
	return &ceremonyLog{out: out}
}

// line prints s and records it (and any embedded newlines as separate rows).
func (c *ceremonyLog) line(s string) {
	fmt.Fprintln(c.out, s)
	c.note(s)
}

// printf formats like fmt.Printf then line-prints (no trailing newline in format).
func (c *ceremonyLog) printf(format string, args ...any) {
	c.line(fmt.Sprintf(format, args...))
}

// note records s without writing to out and without touching $IRIS_CEREMONY_LOG.
// Use after a Bubble Tea progress bar has already drawn its final line (and
// runProgressBar has already mirrored that line into the shared log file).
func (c *ceremonyLog) note(s string) {
	for _, row := range strings.Split(s, "\n") {
		c.lines = append(c.lines, row)
	}
}

// content returns the full transcript (newline-joined).
func (c *ceremonyLog) content() string {
	return strings.Join(c.lines, "\n")
}

// appendCeremonyLogFile mirrors one transcript row into $IRIS_CEREMONY_LOG when set.
func appendCeremonyLogFile(row string) {
	path := strings.TrimSpace(os.Getenv(ceremonyLogPathEnv))
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, row)
	_ = f.Close()
}

// ceremonyReviewDisabled reports whether automatic post-ceremony scrollback
// must not open. Default is disabled (no blocking pager). Enable only with
// IRIS_CEREMONY_REVIEW set; IRIS_NO_CEREMONY_REVIEW still forces off.
func ceremonyReviewDisabled() bool {
	if strings.TrimSpace(os.Getenv(ceremonyNoReviewEnv)) != "" {
		return true
	}
	// Opt-in only: install/uninstall/setup must exit without waiting for "q".
	if strings.TrimSpace(os.Getenv(ceremonyReviewEnv)) == "" {
		return true
	}
	// Parent installer owns the full transcript when the log path is set.
	if strings.TrimSpace(os.Getenv(ceremonyLogPathEnv)) != "" {
		return true
	}
	return false
}
