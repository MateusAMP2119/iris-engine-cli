package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MateusAMP2119/iris-lakehouse/internal/update"
)

// esc is the ANSI escape byte; its presence in captured output means a color code
// leaked. Plain (non-terminal / --json) output must never contain it.
const esc = "\x1b"

// unsetNoColor guarantees NO_COLOR is unset for the duration of the test and
// restored afterward, so the ceremony's NO_COLOR gate is exercised from a known
// baseline regardless of the ambient environment.
func unsetNoColor(t *testing.T) {
	t.Helper()
	t.Setenv("NO_COLOR", "") // registers restoration of the prior value on cleanup
	_ = os.Unsetenv("NO_COLOR")
}

// TestLifecycleCeremonyTTYGating proves the styling helper activates only when
// stdout is a terminal AND --json is off AND NO_COLOR is unset; any one gate
// against it yields a disabled painter that injects no escape codes.
func TestLifecycleCeremonyTTYGating(t *testing.T) {
	cases := []struct {
		name     string
		jsonMode bool
		noColor  bool
		tty      bool
		want     bool
	}{
		{"tty, no json, no NO_COLOR -> styled", false, false, true, true},
		{"not a tty -> plain", false, false, false, false},
		{"--json forces plain even on a tty", true, false, true, false},
		{"NO_COLOR forces plain even on a tty", false, true, true, false},
		{"every gate against styling", true, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			} else {
				unsetNoColor(t)
			}
			p := makePainter(tc.jsonMode, func() bool { return tc.tty })
			if p.enabled != tc.want {
				t.Fatalf("painter enabled = %v, want %v", p.enabled, tc.want)
			}
			got := p.green("OK")
			if hasEsc := strings.Contains(got, esc); hasEsc != tc.want {
				t.Errorf("green(%q) = %q; contains ESC = %v, want %v", "OK", got, hasEsc, tc.want)
			}
			if !tc.want && got != "OK" {
				t.Errorf("disabled painter altered text: green(%q) = %q, want unchanged", "OK", got)
			}
		})
	}
}

// TestLifecycleCeremonyPlainWhenPiped proves no ANSI escape ever reaches a
// non-terminal consumer: with a buffer stdout (the default, not a char device)
// and under --json, every update and uninstall output path is byte-identical
// plain text, and the pinned strings are unchanged. It also proves the converse:
// forcing the tty seam on turns the ceremony on (escapes appear), so the plain
// guarantee is a real gate, not a dead helper.
func TestLifecycleCeremonyPlainWhenPiped(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	deadSock := shortSocket(t)

	t.Run("update up-to-date piped is plain", func(t *testing.T) {
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.runUpdate = func(_ context.Context, current string, _ bool) (update.Result, error) {
			return update.Result{Status: update.StatusUpToDate, From: current, To: "v1.2.3"}, nil
		}
		if code := a.run([]string{"update"}); code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, errb.String())
		}
		assertNoEsc(t, out.String())
		if !strings.Contains(out.String(), "iris is already up to date (version v1.2.3)") {
			t.Errorf("plain up-to-date string changed: %q", out.String())
		}
	})

	t.Run("update replaced piped is plain", func(t *testing.T) {
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.runUpdate = func(_ context.Context, _ string, _ bool) (update.Result, error) {
			return update.Result{Status: update.StatusUpdated, From: "v1.0.0", To: "v2.0.0", Path: "/opt/iris/bin/iris"}, nil
		}
		if code := a.run([]string{"update"}); code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, errb.String())
		}
		assertNoEsc(t, out.String())
		if !strings.Contains(out.String(), "updated iris v1.0.0 -> v2.0.0") {
			t.Errorf("plain updated string changed: %q", out.String())
		}
	})

	t.Run("update --json is plain", func(t *testing.T) {
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true } // even a tty must stay plain under --json
		a.runUpdate = func(_ context.Context, _ string, _ bool) (update.Result, error) {
			return update.Result{Status: update.StatusUpdated, From: "v1.0.0", To: "v2.0.0"}, nil
		}
		if code := a.run([]string{"update", "--json"}); code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, errb.String())
		}
		assertNoEsc(t, out.String())
	})

	t.Run("uninstall success piped is plain", func(t *testing.T) {
		scratch := scratchExecutable(t)
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.executablePath = func() (string, error) { return scratch, nil }
		if code := a.run([]string{"--socket", deadSock, "uninstall", "--yes"}); code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, errb.String())
		}
		assertNoEsc(t, out.String())
		if !strings.Contains(out.String(), "[3/3]") || !strings.Contains(out.String(), "Binary removed") {
			t.Errorf("plain uninstall step lines changed: %q", out.String())
		}
		if strings.Contains(out.String(), "█") || strings.Contains(out.String(), "░") {
			t.Errorf("plain uninstall must carry no banner or progress bar: %q", out.String())
		}
	})

	t.Run("uninstall abort piped is plain", func(t *testing.T) {
		scratch := scratchExecutable(t)
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.executablePath = func() (string, error) { return scratch, nil }
		a.confirm = func(_ string, _ bool) (bool, error) { return false, nil }
		if code := a.run([]string{"--socket", deadSock, "uninstall"}); code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, errb.String())
		}
		assertNoEsc(t, out.String())
		if !strings.Contains(out.String(), "Aborted. Nothing removed.") {
			t.Errorf("plain abort string changed: %q", out.String())
		}
	})

	t.Run("update progress stages carry no ANSI when piped", func(t *testing.T) {
		var out bytes.Buffer
		a := newApp(&out, io.Discard)
		p := makePainter(false, func() bool { return false })
		a.renderUpdateStage(p, update.StageResolve, "v9.9.9")
		a.renderUpdateStage(p, update.StageDownload, "iris_x_y.tar.gz\t5.8 MB")
		a.renderUpdateStage(p, update.StageVerify, "OK")
		a.renderUpdateStage(p, update.StageReplace, "done")
		assertNoEsc(t, out.String())
	})

	t.Run("forced tty turns the update ceremony on", func(t *testing.T) {
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.runUpdate = func(_ context.Context, current string, _ bool) (update.Result, error) {
			return update.Result{Status: update.StatusUpToDate, From: current, To: "v1.2.3"}, nil
		}
		if code := a.run([]string{"update"}); code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, errb.String())
		}
		if !strings.Contains(out.String(), esc) {
			t.Errorf("forced-tty update emitted no escape: %q", out.String())
		}
	})

	t.Run("forced tty turns the uninstall ceremony on", func(t *testing.T) {
		scratch := scratchExecutable(t)
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		a.isTTY = func() bool { return true }
		a.executablePath = func() (string, error) { return scratch, nil }
		if code := a.run([]string{"--socket", deadSock, "uninstall", "--yes"}); code != exitOK {
			t.Fatalf("exit = %d, want %d\n%s", code, exitOK, errb.String())
		}
		if !strings.Contains(out.String(), esc) {
			t.Errorf("forced-tty uninstall emitted no escape: %q", out.String())
		}
	})

	t.Run("update progress stages colored on a tty", func(t *testing.T) {
		var out bytes.Buffer
		a := newApp(&out, io.Discard)
		p := makePainter(false, func() bool { return true })
		a.renderUpdateStage(p, update.StageResolve, "v9.9.9")
		if !strings.Contains(out.String(), esc) {
			t.Errorf("tty progress stage emitted no escape: %q", out.String())
		}
	})
}

// ansiSeq matches an SGR escape sequence, for measuring a styled line's visible
// width (escapes must not count toward alignment).
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// TestUninstallStepLinesAlign proves the staged step lines share one [✓] column
// for any bullet text length, measured on the unstyled text so the green mark
// never shifts it.
func TestUninstallStepLinesAlign(t *testing.T) {
	texts := []string{"Binary removed", "Iris engine stopped successfully.", "No engine state on disk; nothing to remove."}
	var out bytes.Buffer
	a := newApp(&out, io.Discard)
	p := makePainter(false, func() bool { return true })
	for _, text := range texts {
		a.uninstallStepDone(p, text)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(texts) {
		t.Fatalf("step lines = %d, want %d:\n%s", len(lines), len(texts), out.String())
	}
	want := -1
	for _, line := range lines {
		plain := stripANSI(line)
		col := strings.Index(plain, "[✓]")
		if col < 0 {
			t.Fatalf("step line missing its [✓] mark: %q", plain)
		}
		if want == -1 {
			want = col
		}
		if col != want {
			t.Errorf("misaligned [✓] column %d, want %d: %q", col, want, plain)
		}
	}
}

// TestFarewellBannerRowsUniform proves the FAREWELL! banner rows paint one color
// per row and stay uniform in visible width.
func TestFarewellBannerRowsUniform(t *testing.T) {
	var out bytes.Buffer
	farewellBanner(&out, makePainter(false, func() bool { return true }))
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(farewellBannerRows) {
		t.Fatalf("banner = %d lines, want %d", len(lines), len(farewellBannerRows))
	}
	want := utf8.RuneCountInString(stripANSI(lines[0]))
	for i, line := range lines {
		if !strings.Contains(line, esc) {
			t.Errorf("banner row %d carries no color: %q", i, line)
		}
		if got := utf8.RuneCountInString(stripANSI(line)); got != want {
			t.Errorf("banner row %d width = %d, want %d", i, got, want)
		}
	}
}

// assertNoEsc fails the test if s carries any ANSI escape byte.
func assertNoEsc(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, esc) {
		t.Errorf("output leaked an ANSI escape to a non-terminal consumer: %q", s)
	}
}
