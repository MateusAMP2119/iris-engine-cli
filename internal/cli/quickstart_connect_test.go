package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// scriptedTourInput returns a tourInputFunc answering from the given lines in
// order, then EOF -- the tour's clean-decline signal.
func scriptedTourInput(lines ...string) tourInputFunc {
	i := 0
	return func(_, _ string) (string, error) {
		if i >= len(lines) {
			return "", io.EOF
		}
		line := lines[i]
		i++
		return line, nil
	}
}

// newRemoteTourSession builds a harnessed tour session whose line and secret
// reads are the scripted answers, over a disabled painter.
func newRemoteTourSession(answers ...string) *tourSession {
	input := scriptedTourInput(answers...)
	return &tourSession{
		ctx:    context.Background(),
		p:      painter{},
		pick:   func(string, int) (int, promptAnswer, error) { return 1, answerProceed, nil },
		input:  input,
		secret: input,
	}
}

// TestTourEngineHomeFork proves the tour's opening fork on the line-dialogue
// surface: 1 (and the empty default) stays local, 2 goes remote, and a decline
// aborts clean.
func TestTourEngineHomeFork(t *testing.T) {
	cases := []struct {
		name    string
		choice  int
		ans     promptAnswer
		remote  bool
		aborted bool
	}{
		{name: "1 is local", choice: 1, ans: answerProceed, remote: false},
		{name: "2 is remote", choice: 2, ans: answerProceed, remote: true},
		{name: "quit aborts", choice: 0, ans: answerQuit, aborted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			s := &tourSession{
				ctx: context.Background(),
				p:   painter{},
				pick: func(question string, n int) (int, promptAnswer, error) {
					if n != 2 {
						t.Errorf("fork asked over %d options, want 2", n)
					}
					if !strings.Contains(question, "1-2") {
						t.Errorf("fork question = %q, want the 1-2 range named", question)
					}
					return tc.choice, tc.ans, nil
				},
			}
			remote, err := a.tourEngineHome(s)
			if tc.aborted {
				if !errors.Is(err, errTourAborted) {
					t.Fatalf("err = %v, want errTourAborted", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("tourEngineHome: %v", err)
			}
			if remote != tc.remote {
				t.Errorf("remote = %v, want %v", remote, tc.remote)
			}
			if !strings.Contains(out.String(), "Where does your engine live?") {
				t.Errorf("fork never rendered its question:\n%s", out.String())
			}
		})
	}
}

// TestTourConnectRemote proves the tour's remote branch end to end against a
// live in-process engine: host and PAT asked, verified, the workspace question
// answered, and the connection recorded in that workspace's .iris/iris.toml.
func TestTourConnectRemote(t *testing.T) {
	clearTargetEnv(t)
	t.Chdir(t.TempDir())
	host := startConnectDaemon(t, "secret")
	ws := filepath.Join(t.TempDir(), "remote-ws")

	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	s := newRemoteTourSession(host, "secret", ws)

	if err := a.tourConnectRemote(s); err != nil {
		t.Fatalf("tourConnectRemote: %v\nstdout: %s\nstderr: %s", err, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "connected to "+host) {
		t.Errorf("stdout misses the connected line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Enjoy iris.") {
		t.Errorf("stdout misses the remote wrap-up:\n%s", out.String())
	}

	res, err := config.LoadTOMLFile(filepath.Join(ws, config.DirName, config.FileName))
	if err != nil {
		t.Fatalf("load recorded iris.toml: %v", err)
	}
	if res.Layer.Host == nil || *res.Layer.Host != host {
		t.Errorf("recorded host = %v, want %s", res.Layer.Host, host)
	}
	if res.Layer.Token == nil || *res.Layer.Token != "secret" {
		t.Errorf("recorded token = %v, want secret", res.Layer.Token)
	}
	st, err := os.Stat(filepath.Join(ws, config.DirName, config.FileName))
	if err != nil {
		t.Fatalf("stat recorded iris.toml: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("iris.toml mode = %o, want 600", got)
	}
}

// TestTourConnectRemoteRetries proves a failed verification explains itself and
// re-asks instead of ending the tour: a rejected PAT on the first attempt, the
// corrected pair on the second, one recorded connection.
func TestTourConnectRemoteRetries(t *testing.T) {
	clearTargetEnv(t)
	t.Chdir(t.TempDir())
	host := startConnectDaemon(t, "right")
	ws := filepath.Join(t.TempDir(), "remote-ws")

	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	s := newRemoteTourSession(host, "wrong", host, "right", ws)

	if err := a.tourConnectRemote(s); err != nil {
		t.Fatalf("tourConnectRemote: %v\nstderr: %s", err, errb.String())
	}
	if !strings.Contains(errb.String(), "rejected the PAT") {
		t.Errorf("stderr misses the rejected-PAT explanation:\n%s", errb.String())
	}
	if !strings.Contains(out.String(), "try again") {
		t.Errorf("stdout misses the retry guidance:\n%s", out.String())
	}
	res, err := config.LoadTOMLFile(filepath.Join(ws, config.DirName, config.FileName))
	if err != nil {
		t.Fatalf("load recorded iris.toml: %v", err)
	}
	if res.Layer.Token == nil || *res.Layer.Token != "right" {
		t.Errorf("recorded token = %v, want the corrected one", res.Layer.Token)
	}
}

// TestTourConnectRemoteAborts proves every remote-branch question honors the
// tour's decline contract: an empty or q answer at the host or PAT question is
// the clean abort, and nothing is recorded.
func TestTourConnectRemoteAborts(t *testing.T) {
	cases := []struct {
		name    string
		answers []string
	}{
		{name: "empty host", answers: []string{""}},
		{name: "q host", answers: []string{"q"}},
		{name: "empty PAT", answers: []string{"db.example:8443", ""}},
		{name: "q PAT", answers: []string{"db.example:8443", "q"}},
		{name: "EOF", answers: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTargetEnv(t)
			ws := t.TempDir()
			t.Chdir(ws)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			s := newRemoteTourSession(tc.answers...)
			if err := a.tourConnectRemote(s); !errors.Is(err, errTourAborted) {
				t.Fatalf("err = %v, want errTourAborted", err)
			}
			if _, err := os.Stat(filepath.Join(ws, config.DirName, config.FileName)); err == nil {
				t.Error("an aborted remote branch still recorded a connection")
			}
		})
	}
}
