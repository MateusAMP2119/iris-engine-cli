package dispatch_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
)

// hostEnvFrom builds a daemon-environment lookup over a fixed map: an absent key
// resolves to the empty string, matching os.Getenv's behaviour for an unset var.
func hostEnvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// readFileFrom builds a fake file reader over an in-memory map of path -> bytes.
// A path absent from the map reads as fs.ErrNotExist, so a resolver's missing-file
// path is exercised without touching the filesystem.
func readFileFrom(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		b, ok := m[p]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(b), nil
	}
}

// TestResolveRunEnvInterpolationMerge proves the declared env map resolves as
// literals or ${HOST_VAR} daemon-environment interpolations, producing the
// deterministic overlay composeEnv merges onto the inherited environment.
func TestResolveRunEnvInterpolationMerge(t *testing.T) {
	// spec: S03/env-interpolation-merge
	host := map[string]string{
		"HOST_TOKEN": "s3cr3t",
		"REGION":     "eu-west-1",
	}
	cases := []struct {
		name     string
		declared map[string]string
		want     []string
	}{
		{
			name:     "literal value passes through verbatim",
			declared: map[string]string{"MODE": "batch"},
			want:     []string{"MODE=batch"},
		},
		{
			name:     "braced interpolation resolves from the daemon environment",
			declared: map[string]string{"TOKEN": "${HOST_TOKEN}"}, //nolint:gosec // G101: synthetic test env fixture, not a real credential
			want:     []string{"TOKEN=s3cr3t"},
		},
		{
			name:     "interpolation embedded in surrounding text",
			declared: map[string]string{"URL": "https://${REGION}.example.com/api"},
			want:     []string{"URL=https://eu-west-1.example.com/api"},
		},
		{
			name:     "unset host var resolves to empty (Compose-style)",
			declared: map[string]string{"MISSING": "${NOT_SET}"},
			want:     []string{"MISSING="},
		},
		{
			name:     "escaped $$ yields a literal dollar",
			declared: map[string]string{"PRICE": "$$5"},
			want:     []string{"PRICE=$5"},
		},
		{
			name:     "bare dollar not followed by brace stays literal",
			declared: map[string]string{"RAW": "cost is $ each"},
			want:     []string{"RAW=cost is $ each"},
		},
		{
			name: "multiple entries emitted sorted by key",
			declared: map[string]string{ //nolint:gosec // G101: synthetic test env fixture, not a real credential
				"ZED":   "last",
				"ALPHA": "first",
				"TOKEN": "${HOST_TOKEN}",
			},
			want: []string{"ALPHA=first", "TOKEN=s3cr3t", "ZED=last"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dispatch.ResolveRunEnv(tc.declared, nil, hostEnvFrom(host), nil)
			if err != nil {
				t.Fatalf("ResolveRunEnv() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ResolveRunEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveRunEnvWinsOverEnvFile proves the explicit env value wins over an
// env_file entry for the same key, while keys unique to either source survive.
func TestResolveRunEnvWinsOverEnvFile(t *testing.T) {
	// spec: S03/env-wins-over-env-file
	host := map[string]string{"HOST_TOKEN": "from-host"}

	t.Run("explicit env overrides env_file on key collision", func(t *testing.T) {
		files := map[string]string{ //nolint:gosec // G101: synthetic test env fixture, not a real credential
			"secrets.env": "SHARED=file-value\nONLY_FILE=f\n",
		}
		declared := map[string]string{"SHARED": "env-value", "ONLY_ENV": "e"}
		got, err := dispatch.ResolveRunEnv(declared, []string{"secrets.env"}, hostEnvFrom(host), readFileFrom(files))
		if err != nil {
			t.Fatalf("ResolveRunEnv() error = %v", err)
		}
		want := []string{"ONLY_ENV=e", "ONLY_FILE=f", "SHARED=env-value"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ResolveRunEnv() = %v, want %v", got, want)
		}
	})

	t.Run("winning env value is still interpolated", func(t *testing.T) {
		files := map[string]string{"secrets.env": "TOKEN=file-token\n"}
		declared := map[string]string{"TOKEN": "${HOST_TOKEN}"} //nolint:gosec // G101: synthetic test env fixture, not a real credential
		got, err := dispatch.ResolveRunEnv(declared, []string{"secrets.env"}, hostEnvFrom(host), readFileFrom(files))
		if err != nil {
			t.Fatalf("ResolveRunEnv() error = %v", err)
		}
		want := []string{"TOKEN=from-host"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ResolveRunEnv() = %v, want %v", got, want)
		}
	})

	t.Run("later env_file overrides earlier on key collision", func(t *testing.T) {
		files := map[string]string{
			"base.env":    "KEY=base\n",
			"overlay.env": "KEY=overlay\n",
		}
		got, err := dispatch.ResolveRunEnv(nil, []string{"base.env", "overlay.env"}, hostEnvFrom(host), readFileFrom(files))
		if err != nil {
			t.Fatalf("ResolveRunEnv() error = %v", err)
		}
		want := []string{"KEY=overlay"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ResolveRunEnv() = %v, want %v", got, want)
		}
	})
}

// TestResolveRunEnvEnvFileFreshPerRun proves env_file contents are re-read at each
// resolve against a real temp file: mutating the file between two resolves changes
// the second result, so no caching survives a run and a change takes effect on the
// next dispatch without re-apply. It also pins the file-read error paths.
func TestResolveRunEnvEnvFileFreshPerRun(t *testing.T) {
	// spec: S03/env-file-fresh-per-run
	host := hostEnvFrom(nil)

	t.Run("a file change is reflected on the next resolve", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "secrets.env")

		if err := os.WriteFile(path, []byte("API_KEY=v1\n"), 0o600); err != nil {
			t.Fatalf("write env_file: %v", err)
		}
		first, err := dispatch.ResolveRunEnv(nil, []string{path}, host, os.ReadFile)
		if err != nil {
			t.Fatalf("first resolve: %v", err)
		}
		if want := []string{"API_KEY=v1"}; !reflect.DeepEqual(first, want) {
			t.Fatalf("first resolve = %v, want %v", first, want)
		}

		// Mutate the file between runs; no re-apply, only a fresh dispatch.
		if err := os.WriteFile(path, []byte("API_KEY=v2\n"), 0o600); err != nil {
			t.Fatalf("rewrite env_file: %v", err)
		}
		second, err := dispatch.ResolveRunEnv(nil, []string{path}, host, os.ReadFile)
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		if want := []string{"API_KEY=v2"}; !reflect.DeepEqual(second, want) {
			t.Errorf("second resolve = %v, want %v (env_file was not re-read fresh)", second, want)
		}
	})

	t.Run("comments and blank lines are ignored", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "secrets.env")
		body := "# a comment\n\nKEY=value\n   # indented comment\nOTHER=two\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write env_file: %v", err)
		}
		got, err := dispatch.ResolveRunEnv(nil, []string{path}, host, os.ReadFile)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		want := []string{"KEY=value", "OTHER=two"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("resolve = %v, want %v", got, want)
		}
	})

	t.Run("a missing env_file errors distinguishably as not-exist", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "absent.env")
		_, err := dispatch.ResolveRunEnv(nil, []string{path}, host, os.ReadFile)
		if err == nil {
			t.Fatal("resolve over a missing env_file returned nil error")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want it to wrap fs.ErrNotExist", err)
		}
	})

	t.Run("a malformed line errors naming the file and line", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.env")
		if err := os.WriteFile(path, []byte("KEY=value\nnot a pair\n"), 0o600); err != nil {
			t.Fatalf("write env_file: %v", err)
		}
		_, err := dispatch.ResolveRunEnv(nil, []string{path}, host, os.ReadFile)
		if err == nil {
			t.Fatal("resolve over a malformed env_file returned nil error")
		}
		msg := err.Error()
		if !strings.Contains(msg, path) || !strings.Contains(msg, ":2") {
			t.Errorf("error = %q, want it to name %s and line 2", msg, path)
		}
	})
}
