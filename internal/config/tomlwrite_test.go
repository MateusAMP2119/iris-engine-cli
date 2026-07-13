package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpsertTOMLFileCreates proves the upsert creates the file and its directory
// when absent, writes 0600, and the parser reads the values back.
func TestUpsertTOMLFileCreates(t *testing.T) {
	// spec: S08/iris-toml-upsert-preserves
	path := filepath.Join(t.TempDir(), DirName, FileName)
	if err := UpsertTOMLFile(path, map[string]string{"host": "db.example:8443", "token": "s3cret"}); err != nil {
		t.Fatalf("UpsertTOMLFile: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}

	res, err := LoadTOMLFile(path)
	if err != nil {
		t.Fatalf("LoadTOMLFile: %v", err)
	}
	if res.Layer.Host == nil || *res.Layer.Host != "db.example:8443" {
		t.Errorf("host read back = %v, want db.example:8443", res.Layer.Host)
	}
	if res.Layer.Token == nil || *res.Layer.Token != "s3cret" {
		t.Errorf("token read back = %v, want s3cret", res.Layer.Token)
	}
}

// TestUpsertTOMLFilePreserves proves an existing file survives the upsert: keys
// outside the set and comments stay verbatim, an owned key is rewritten in
// place, and a duplicate of an owned key is dropped so the rewrite cannot be
// overridden by a later line (the parser is last-one-wins).
func TestUpsertTOMLFilePreserves(t *testing.T) {
	// spec: S08/iris-toml-upsert-preserves
	path := filepath.Join(t.TempDir(), FileName)
	prior := strings.Join([]string{
		"# workspace engine settings",
		`pg_dsn = "postgres://iris@localhost/iris"`,
		"",
		`host = "old.example:1"`,
		"retain = 25",
		`host = "older.example:2"`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := UpsertTOMLFile(path, map[string]string{"host": "new.example:8443", "token": "tok"}); err != nil {
		t.Fatalf("UpsertTOMLFile: %v", err)
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads back the file it wrote under its own TempDir.
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"# workspace engine settings",
		`pg_dsn = "postgres://iris@localhost/iris"`,
		"retain = 25",
		`host = "new.example:8443"`,
		`token = "tok"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("written file misses %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "old.example") || strings.Contains(content, "older.example") {
		t.Errorf("written file still carries a stale host line:\n%s", content)
	}
	if got := strings.Count(content, "host ="); got != 1 {
		t.Errorf("host lines = %d, want 1 (duplicates dropped):\n%s", got, content)
	}

	res, err := LoadTOMLFile(path)
	if err != nil {
		t.Fatalf("LoadTOMLFile: %v", err)
	}
	if res.Layer.Host == nil || *res.Layer.Host != "new.example:8443" {
		t.Errorf("host read back = %v, want new.example:8443", res.Layer.Host)
	}
	if res.Layer.PgDSN == nil || *res.Layer.PgDSN != "postgres://iris@localhost/iris" {
		t.Errorf("pg_dsn read back = %v, want preserved", res.Layer.PgDSN)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode after upsert = %o, want 600 (tightened from 644)", got)
	}
}

// TestUpsertTOMLFileEscapes proves a value carrying the parser's two escapes
// (backslash, double quote) round-trips through the writer.
func TestUpsertTOMLFileEscapes(t *testing.T) {
	// spec: S08/iris-toml-upsert-preserves
	path := filepath.Join(t.TempDir(), FileName)
	value := `to"ken\with escapes`
	if err := UpsertTOMLFile(path, map[string]string{"token": value}); err != nil {
		t.Fatalf("UpsertTOMLFile: %v", err)
	}
	res, err := LoadTOMLFile(path)
	if err != nil {
		t.Fatalf("LoadTOMLFile: %v", err)
	}
	if res.Layer.Token == nil || *res.Layer.Token != value {
		t.Errorf("token read back = %v, want %q", res.Layer.Token, value)
	}
}

// TestUpsertTOMLFileRejectsBadKey proves a non-flat key is refused before
// anything is written.
func TestUpsertTOMLFileRejectsBadKey(t *testing.T) {
	// spec: S08/iris-toml-upsert-preserves
	path := filepath.Join(t.TempDir(), FileName)
	if err := UpsertTOMLFile(path, map[string]string{"a.b": "x"}); err == nil {
		t.Fatal("UpsertTOMLFile accepted a dotted key")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a rejected upsert still wrote the file")
	}
}
