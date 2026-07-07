package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestPATCreateScopeValidation proves `iris pat create` validates its scope set
// locally before reaching the leader (specification section 7): it accepts any
// non-empty subset of {control, read, data} and rejects an empty set or an unknown
// scope as a usage error (exit 2), and it rejects --read/--endpoint without the data
// scope. A valid request gets past validation and then requires a running daemon
// (exit 3 against an unreachable socket), proving validation is the only local gate.
//
// spec: S07/pat-scope-subset-validation
func TestPATCreateScopeValidation(t *testing.T) {
	t.Run("S07/pat-scope-subset-validation", func(t *testing.T) {
		// Isolate ambient IRIS_* config so a developer's exported socket/host cannot
		// redirect the resolved dial target: the --socket flag is the only target.
		t.Setenv("IRIS_SOCKET", "")
		t.Setenv("IRIS_HOST", "")
		t.Setenv("IRIS_TOKEN", "")

		sock := shortSocket(t) // nothing listening: a valid request lands on no-daemon.

		cases := []struct {
			name string
			args []string
			want int
		}{
			{"no scope is a usage error", []string{"pat", "create"}, exitUsage},
			{"empty scope value is a usage error", []string{"pat", "create", "--scope", ""}, exitUsage},
			{"unknown scope is a usage error", []string{"pat", "create", "--scope", "admin"}, exitUsage},
			{"read grant without data scope is a usage error",
				[]string{"pat", "create", "--scope", "control", "--read", "analytics.orders.amount"}, exitUsage},
			{"endpoint grant without data scope is a usage error",
				[]string{"pat", "create", "--scope", "read", "--endpoint", "orders_by_customer"}, exitUsage},
			{"a single valid scope passes validation, then needs the leader",
				[]string{"--socket", sock, "pat", "create", "--scope", "control"}, exitNoDaemon},
			{"a valid subset passes validation, then needs the leader",
				[]string{"--socket", sock, "pat", "create", "--scope", "read", "--scope", "data"}, exitNoDaemon},
			{"a data scope with read grants passes validation, then needs the leader",
				[]string{"--socket", sock, "pat", "create", "--scope", "data", "--read", "analytics.orders.amount"}, exitNoDaemon},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var out, errb bytes.Buffer
				code := newApp(&out, &errb).run(tc.args)
				if code != tc.want {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.want, out.String(), errb.String())
				}
			})
		}
	})
}

// TestPATCreateUnknownScopeNamesIt proves the usage error names the offending scope,
// so an operator sees which token was rejected.
//
// spec: S07/pat-scope-subset-validation
func TestPATCreateUnknownScopeNamesIt(t *testing.T) {
	t.Run("S07/pat-scope-subset-validation", func(t *testing.T) {
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"pat", "create", "--scope", "superuser"})
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitUsage, errb.String())
		}
		if !strings.Contains(errb.String(), "superuser") {
			t.Errorf("usage error does not name the unknown scope:\n%s", errb.String())
		}
	})
}
