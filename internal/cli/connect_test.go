package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pat"
)

// startConnectDaemon stands up an in-process daemon with a TCP listener that
// accepts one bearer token, returning its host:port.
func startConnectDaemon(t *testing.T, goodToken string) string {
	t.Helper()
	srv := daemon.NewServer(
		config.Settings{Socket: shortSocket(t), TCP: "127.0.0.1:0"},
		api.NewMux(),
		daemon.WithVerifier(acceptTokenVerifier{good: goodToken}),
	)
	startInProcess(t, srv)
	return srv.TCPAddr()
}

// readEngineHomeTOML loads the engine home's iris.toml.
func readEngineHomeTOML(t *testing.T, home string) config.TOML {
	t.Helper()
	res, err := config.LoadTOMLFile(filepath.Join(home, config.FileName))
	if err != nil {
		t.Fatalf("load written iris.toml: %v", err)
	}
	return res
}

// TestEngineConnectRecords proves the happy path: `iris engine connect <host>
// --token <pat>` verifies the pair against the live engine and records both in
// the engine home's iris.toml (0600), reporting the engine's role. The command
// runs from an unrelated directory: recording is cwd-independent.
func TestEngineConnectRecords(t *testing.T) {
	home := clearTargetEnv(t)
	t.Chdir(t.TempDir())
	host := startConnectDaemon(t, "secret")

	var out, errb bytes.Buffer
	code := newApp(&out, &errb).run([]string{"engine", "connect", host, "--token", "secret"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "connected to "+host) {
		t.Errorf("stdout misses the connected line:\n%s", out.String())
	}

	res := readEngineHomeTOML(t, home)
	if res.Layer.Host == nil || *res.Layer.Host != host {
		t.Errorf("recorded host = %v, want %s", res.Layer.Host, host)
	}
	if res.Layer.Token == nil || *res.Layer.Token != "secret" {
		t.Errorf("recorded token = %v, want secret", res.Layer.Token)
	}
	st, err := os.Stat(filepath.Join(home, config.FileName))
	if err != nil {
		t.Fatalf("stat iris.toml: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("iris.toml mode = %o, want 600", got)
	}
}

// TestEngineConnectJSON proves the --json surface: one data envelope carrying
// the host, role, and config path -- and never the token.
func TestEngineConnectJSON(t *testing.T) {
	home := clearTargetEnv(t)
	t.Chdir(t.TempDir())
	host := startConnectDaemon(t, "secret")

	var out, errb bytes.Buffer
	code := newApp(&out, &errb).run([]string{"engine", "connect", host, "--token", "secret", "--json"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
	}
	var env struct {
		Data connectResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v\nstdout: %s", err, out.String())
	}
	if env.Data.Host != host {
		t.Errorf("envelope host = %q, want %q", env.Data.Host, host)
	}
	if env.Data.Config != filepath.Join(home, config.FileName) {
		t.Errorf("envelope config = %q, want the engine home iris.toml", env.Data.Config)
	}
	if strings.Contains(out.String(), "secret") {
		t.Errorf("the token leaked into the --json envelope:\n%s", out.String())
	}
}

// TestEngineConnectTLS proves connect is scheme-aware like every TCP dial: an
// https:// host verifies and records against a TLS-serving engine.
func TestEngineConnectTLS(t *testing.T) {
	home := clearTargetEnv(t)
	t.Chdir(t.TempDir())
	certFile, keyFile, pool := selfSignedCert(t)
	srv := daemon.NewServer(
		config.Settings{Socket: shortSocket(t), TCP: "127.0.0.1:0", TLSCert: certFile, TLSKey: keyFile},
		api.NewMux(),
		daemon.WithVerifier(acceptTokenVerifier{good: "secret"}),
	)
	startInProcess(t, srv)

	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	a.daemonTLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	code := a.run([]string{"engine", "connect", "https://" + srv.TCPAddr(), "--token", "secret"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
	}
	res := readEngineHomeTOML(t, home)
	if res.Layer.Host == nil || *res.Layer.Host != "https://"+srv.TCPAddr() {
		t.Errorf("recorded host = %v, want the https:// form kept", res.Layer.Host)
	}
}

// TestEngineConnectRejectedPAT proves a live engine rejecting the PAT is an
// operation failure (exit 4) that names the fix and records nothing.
func TestEngineConnectRejectedPAT(t *testing.T) {
	home := clearTargetEnv(t)
	t.Chdir(t.TempDir())
	host := startConnectDaemon(t, "right")

	var out, errb bytes.Buffer
	code := newApp(&out, &errb).run([]string{"engine", "connect", host, "--token", "wrong"})
	if code != exitOpFailed {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOpFailed, errb.String())
	}
	if !strings.Contains(errb.String(), "rejected the PAT") {
		t.Errorf("stderr misses the rejected-PAT guidance:\n%s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, config.FileName)); err == nil {
		t.Error("a rejected PAT still recorded a connection")
	}
}

// controlOnlyVerifier accepts one bearer token with control scope only, so a
// connect probe passes the PAT gate but fails the read-scope check (403).
type controlOnlyVerifier struct{ good string }

// VerifyToken accepts only the configured token, resolving it to a
// control-only authority.
func (v controlOnlyVerifier) VerifyToken(_ context.Context, tok string) (api.Authority, error) {
	if tok == v.good {
		return api.Authority{PATID: "probe", Scopes: []pat.Scope{pat.ScopeControl}}, nil
	}
	return api.Authority{}, errors.New("cli: bad token")
}

// TestEngineConnectMissingScope proves a PAT the engine accepts but that lacks
// the read scope is its own operation failure naming the scope to mint, and
// records nothing -- scopes do not imply each other, so a control-only PAT
// cannot pass the read-gated probe.
func TestEngineConnectMissingScope(t *testing.T) {
	home := clearTargetEnv(t)
	t.Chdir(t.TempDir())
	srv := daemon.NewServer(
		config.Settings{Socket: shortSocket(t), TCP: "127.0.0.1:0"},
		api.NewMux(),
		daemon.WithVerifier(controlOnlyVerifier{good: "control-pat"}),
	)
	startInProcess(t, srv)

	var out, errb bytes.Buffer
	code := newApp(&out, &errb).run([]string{"engine", "connect", srv.TCPAddr(), "--token", "control-pat"})
	if code != exitOpFailed {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOpFailed, errb.String())
	}
	if !strings.Contains(errb.String(), "lacks the read scope") {
		t.Errorf("stderr misses the missing-scope guidance:\n%s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, config.FileName)); err == nil {
		t.Error("a scope-refused PAT still recorded a connection")
	}
}

// TestEngineConnectUnreachable proves an unreachable host is no-daemon (exit 3)
// and records nothing.
func TestEngineConnectUnreachable(t *testing.T) {
	home := clearTargetEnv(t)
	t.Chdir(t.TempDir())

	var out, errb bytes.Buffer
	code := newApp(&out, &errb).run([]string{"engine", "connect", "127.0.0.1:1", "--token", "secret"})
	if code != exitNoDaemon {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitNoDaemon, errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, config.FileName)); err == nil {
		t.Error("an unreachable host still recorded a connection")
	}
}

// TestEngineConnectUsage proves the non-interactive missing-input paths are
// usage errors (exit 2) naming what to pass.
func TestEngineConnectUsage(t *testing.T) {
	clearTargetEnv(t)
	t.Chdir(t.TempDir())

	t.Run("missing host", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"engine", "connect"})
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d", code, exitUsage)
		}
		if !strings.Contains(errb.String(), "needs the engine host") {
			t.Errorf("stderr misses the host guidance:\n%s", errb.String())
		}
	})

	t.Run("missing token", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"engine", "connect", "db.example:8443"})
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d", code, exitUsage)
		}
		if !strings.Contains(errb.String(), "needs a PAT") {
			t.Errorf("stderr misses the PAT guidance:\n%s", errb.String())
		}
	})
}

// TestEngineConnectPrompts proves the interactive path: with a terminal pair
// and neither host nor token supplied, connect asks for both through the
// injectable seams -- the host as a plain line, the PAT as the hidden read.
func TestEngineConnectPrompts(t *testing.T) {
	home := clearTargetEnv(t)
	t.Chdir(t.TempDir())
	host := startConnectDaemon(t, "secret")

	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	a.isTTY = func() bool { return true }
	a.stdinIsTTY = func() bool { return true }
	var askedHost, askedSecret string
	a.connectInput = func(prompt string) (string, error) {
		askedHost = prompt
		return host, nil
	}
	a.connectSecret = func(prompt string) (string, error) {
		askedSecret = prompt
		return "secret", nil
	}
	code := a.run([]string{"engine", "connect"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
	}
	if !strings.Contains(askedHost, "Engine host") {
		t.Errorf("host prompt = %q, want it to name the engine host", askedHost)
	}
	if !strings.Contains(askedSecret, "PAT") {
		t.Errorf("secret prompt = %q, want it to name the PAT", askedSecret)
	}
	res := readEngineHomeTOML(t, home)
	if res.Layer.Host == nil || *res.Layer.Host != host {
		t.Errorf("recorded host = %v, want %s", res.Layer.Host, host)
	}
}

// TestEngineConnectUpserts proves a re-connect rewrites host and token in an
// existing iris.toml while every other line survives verbatim.
func TestEngineConnectUpserts(t *testing.T) {
	home := clearTargetEnv(t)
	t.Chdir(t.TempDir())
	host := startConnectDaemon(t, "secret")

	tomlPath := filepath.Join(home, config.FileName)
	if err := os.MkdirAll(filepath.Dir(tomlPath), 0o755); err != nil {
		t.Fatalf("mkdir the engine home: %v", err)
	}
	prior := "# kept comment\npg_dsn = \"postgres://iris@localhost/iris\"\nhost = \"stale.example:1\"\n"
	if err := os.WriteFile(tomlPath, []byte(prior), 0o600); err != nil {
		t.Fatalf("seed iris.toml: %v", err)
	}

	var out, errb bytes.Buffer
	code := newApp(&out, &errb).run([]string{"engine", "connect", host, "--token", "secret"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
	}
	data, err := os.ReadFile(tomlPath) //nolint:gosec // G304: the test reads back the file it seeded under its own TempDir.
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# kept comment") || !strings.Contains(content, "pg_dsn = ") {
		t.Errorf("re-connect dropped pre-existing lines:\n%s", content)
	}
	if strings.Contains(content, "stale.example") {
		t.Errorf("re-connect kept the stale host:\n%s", content)
	}
	res := readEngineHomeTOML(t, home)
	if res.Layer.Host == nil || *res.Layer.Host != host {
		t.Errorf("recorded host = %v, want %s", res.Layer.Host, host)
	}
}
