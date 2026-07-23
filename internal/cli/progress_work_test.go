package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunProgressWhileNonTTYRunsWork(t *testing.T) {
	var out bytes.Buffer
	ran := false
	err := runProgressWhile(&out, "• Installing engine", func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("runProgressWhile: %v", err)
	}
	if !ran {
		t.Fatal("work did not run")
	}
	// Non-TTY: no bar frames on out.
	if out.Len() != 0 {
		t.Fatalf("non-TTY out = %q, want empty", out.String())
	}
}

func TestRunProgressWhileNonTTYSurfacesError(t *testing.T) {
	boom := errors.New("install failed")
	err := runProgressWhile(&bytes.Buffer{}, "• Installing engine", func() error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestWorkProgressCreepsThenCompletes(t *testing.T) {
	done := make(chan error, 1)
	m := newWorkProgressModel("• Installing engine", done)
	// Poll a few times before work finishes: percent should rise but stay under 100.
	for i := 0; i < 5; i++ {
		next, _ := m.Update(workPollMsg{})
		m = next.(workProgressModel)
	}
	if m.percent <= 0 || m.percent >= 1 {
		t.Fatalf("after polls percent = %v, want (0, 1)", m.percent)
	}
	if m.percent > 0.92 {
		t.Fatalf("percent = %v, want capped at ~0.92 while working", m.percent)
	}
	done <- nil
	// Deliver completion via the same path waitWork would.
	next, cmd := m.Update(workResultMsg{err: nil})
	m = next.(workProgressModel)
	if m.percent != 1 || !m.quitting {
		t.Fatalf("after result: percent=%v quitting=%v", m.percent, m.quitting)
	}
	if cmd == nil {
		t.Fatal("expected progressDone batch cmd")
	}
	// Drain progressDone → Quit
	next, cmd = m.Update(progressDone{})
	_ = next
	// cmd should be tea.Quit — not easily comparable; just ensure no panic.
	_ = cmd
	view := m.View()
	if !strings.Contains(view, "Installing engine") {
		t.Fatalf("view missing label: %q", view)
	}
	if !strings.Contains(view, "100%") {
		t.Fatalf("view missing 100%%: %q", view)
	}
}

func TestWorkProgressShowsElapsedOnLongJobs(t *testing.T) {
	m := newWorkProgressModel("• Installing engine", make(chan error, 1))
	// Fresh job: no elapsed suffix yet.
	if v := m.View(); strings.Contains(v, "(") {
		t.Fatalf("fresh view already shows elapsed: %q", v)
	}
	// A job past the threshold ticks elapsed time beside the label.
	m.start = time.Now().Add(-70 * time.Second)
	if v := m.View(); !strings.Contains(v, "(1m10s)") {
		t.Fatalf("view = %q, want elapsed (1m10s)", v)
	}
	// A settled bar drops the counter.
	m.quitting = true
	m.percent = 1
	if v := m.View(); strings.Contains(v, "1m10s") {
		t.Fatalf("settled view still shows elapsed: %q", v)
	}
}

func TestFormatWorkElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{7 * time.Second, "7s"},
		{59 * time.Second, "59s"},
		{65 * time.Second, "1m05s"},
		{3 * time.Minute, "3m00s"},
	}
	for _, tc := range cases {
		if got := formatWorkElapsed(tc.d); got != tc.want {
			t.Errorf("formatWorkElapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestWaitWorkReturnsResultOrPoll(t *testing.T) {
	done := make(chan error, 1)
	// Empty channel → poll after timeout.
	start := time.Now()
	msg := waitWork(done)()
	if _, ok := msg.(workPollMsg); !ok {
		t.Fatalf("msg type %T, want workPollMsg", msg)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("waitWork returned too fast without waiting")
	}
	done <- errors.New("x")
	msg = waitWork(done)()
	wr, ok := msg.(workResultMsg)
	if !ok || wr.err == nil || wr.err.Error() != "x" {
		t.Fatalf("msg = %#v, want workResultMsg with err x", msg)
	}
}
