package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// manifestYAML renders a minimal valid tool manifest for this platform,
// pinning the given digest.
func manifestYAML(name, version, digest string) string {
	return fmt.Sprintf(`name: %s
version: "%s"
kind: tool
verbs:
  send:
    timeout_seconds: 5
binaries:
  %s:
    url: ./bin
    sha256: "%s"
`, name, version, Platform(), digest)
}

// writeInstallSource lays a manifest and its binary in a temp dir and returns
// the manifest path.
func writeInstallSource(t *testing.T, name, version string, binary []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bin"), binary, 0o755); err != nil { //nolint:gosec // test fixture binary
		t.Fatal(err)
	}
	path := filepath.Join(dir, ManifestFile)
	if err := os.WriteFile(path, []byte(manifestYAML(name, version, Digest(binary))), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseManifest(t *testing.T) {
	valid := manifestYAML("mailer", "1.0", strings.Repeat("ab", 32))
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{"valid", valid, ""},
		{"empty", "", "empty manifest"},
		{"unknown top-level field", valid + "extra: 1\n", `unknown field "extra"`},
		{"bad name", strings.Replace(valid, "name: mailer", "name: ../evil", 1), "not a lowercase slug"},
		{"bad version", strings.Replace(valid, `version: "1.0"`, `version: "../up"`, 1), "not a path-safe token"},
		{"bad kind", strings.Replace(valid, "kind: tool", "kind: daemon", 1), `kind "daemon"`},
		{"no verbs", strings.Replace(valid, "  send:\n    timeout_seconds: 5\n", "", 1), "declares no verbs"},
		{"bad verb name", strings.Replace(valid, "  send:", "  Send.Now:", 1), "not a lowercase slug"},
		{"unknown verb field", strings.Replace(valid, "timeout_seconds: 5", "retries: 5", 1), `unknown field "retries"`},
		{"negative timeout", strings.Replace(valid, "timeout_seconds: 5", "timeout_seconds: -1", 1), "must not be negative"},
		{"bad platform key", strings.Replace(valid, Platform()+":", "somewhere:", 1), "not goos/goarch"},
		{"bad sha256", strings.Replace(valid, strings.Repeat("ab", 32), "beef", 1), "not a 64-hex-digit digest"},
		{"unknown binary field", strings.Replace(valid, "url: ./bin", "path: ./bin", 1), `unknown field "path"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tt.doc))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseManifest: %v", err)
				}
				if m.Name != "mailer" || m.Version != "1.0" || m.Kind != KindTool {
					t.Fatalf("parsed %+v", m)
				}
				if got := m.Verbs["send"].Timeout(); got != 5 {
					t.Fatalf("verb timeout = %d, want 5", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerbTimeoutDefault(t *testing.T) {
	if got := (Verb{}).Timeout(); got != DefaultVerbTimeoutSeconds {
		t.Fatalf("default timeout = %d, want %d", got, DefaultVerbTimeoutSeconds)
	}
}

func TestParseRef(t *testing.T) {
	ref, err := ParseRef("smtp-send@1.0")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if ref.Name != "smtp-send" || ref.Version != "1.0" || ref.String() != "smtp-send@1.0" {
		t.Fatalf("parsed %+v", ref)
	}
	for _, bad := range []string{"noversion", "Bad@1.0", "ok@../up", "@1.0", "x@"} {
		if _, err := ParseRef(bad); err == nil {
			t.Fatalf("ParseRef(%q) accepted", bad)
		}
	}
}

func TestInstallResolveListRemove(t *testing.T) {
	root := t.TempDir()
	binary := []byte("#!/bin/sh\necho hi\n")
	src := writeInstallSource(t, "mailer", "1.0", binary)

	res, err := Install(context.Background(), root, src, nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Digest != Digest(binary) {
		t.Fatalf("install digest = %s", res.Digest)
	}
	if res.Binary != BinaryPath(root, "mailer", "1.0") {
		t.Fatalf("install binary path = %s", res.Binary)
	}
	// Windows has no executable bit; presence is the whole contract there.
	info, err := os.Stat(res.Binary)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0) {
		t.Fatalf("installed binary not executable: %v %v", info, err)
	}

	got, err := Resolve(root, Ref{Name: "mailer", Version: "1.0"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Digest != res.Digest || got.Manifest.Kind != KindTool {
		t.Fatalf("resolved %+v", got)
	}

	entries, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "mailer" || entries[0].Err != nil ||
		len(entries[0].Verbs) != 1 || entries[0].Verbs[0] != "send" {
		t.Fatalf("entries = %+v", entries)
	}

	// A tampered binary must fail resolve and surface on list.
	if err := os.WriteFile(res.Binary, []byte("swapped"), 0o755); err != nil { //nolint:gosec // test tamper
		t.Fatal(err)
	}
	if _, err := Resolve(root, Ref{Name: "mailer", Version: "1.0"}); err == nil || !strings.Contains(err.Error(), "deviates") {
		t.Fatalf("tampered resolve err = %v", err)
	}
	entries, err = List(root)
	if err != nil || len(entries) != 1 || entries[0].Err == nil {
		t.Fatalf("tampered entries = %+v, err %v", entries, err)
	}

	if err := Remove(root, "mailer", "1.0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := Remove(root, "mailer", ""); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("second remove err = %v", err)
	}
	entries, err = List(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("post-remove entries = %+v, err %v", entries, err)
	}
}

func TestInstallRefusals(t *testing.T) {
	root := t.TempDir()
	binary := []byte("bin")

	t.Run("digest mismatch", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bin"), binary, 0o755); err != nil { //nolint:gosec // test fixture binary
			t.Fatal(err)
		}
		doc := manifestYAML("mailer", "1.0", strings.Repeat("00", 32))
		path := filepath.Join(dir, ManifestFile)
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Install(context.Background(), root, path, nil)
		if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(Dir(root, "mailer", "1.0")); !os.IsNotExist(statErr) {
			t.Fatalf("failed install left layout behind: %v", statErr)
		}
	})

	t.Run("service kind installs", func(t *testing.T) {
		src := writeInstallSource(t, "browser", "0.4", binary)
		raw, err := os.ReadFile(src) //nolint:gosec // test-owned temp path
		if err != nil {
			t.Fatal(err)
		}
		doc := strings.Replace(string(raw), "kind: tool", "kind: service", 1)
		if err := os.WriteFile(src, []byte(doc), 0o644); err != nil { //nolint:gosec // test-owned temp path
			t.Fatal(err)
		}
		res, err := Install(context.Background(), root, src, nil)
		if err != nil {
			t.Fatalf("service install: %v", err)
		}
		if res.Manifest.Kind != KindService {
			t.Fatalf("installed kind = %q, want service", res.Manifest.Kind)
		}
	})

	t.Run("remote manifest with relative binary refused", func(t *testing.T) {
		doc := manifestYAML("mailer", "1.0", Digest(binary))
		fetch := func(_ context.Context, url string) ([]byte, error) {
			if url == "https://example.com/manifest.yaml" {
				return []byte(doc), nil
			}
			return nil, fmt.Errorf("unexpected fetch %s", url)
		}
		_, err := Install(context.Background(), root, "https://example.com/manifest.yaml", fetch)
		if err == nil || !strings.Contains(err.Error(), "must pin remote binaries") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestInstallFromURL(t *testing.T) {
	root := t.TempDir()
	binary := []byte("remote binary bytes")
	doc := strings.Replace(manifestYAML("mailer", "2.0", Digest(binary)),
		"url: ./bin", "url: https://example.com/mailer", 1)
	fetch := func(_ context.Context, url string) ([]byte, error) {
		switch url {
		case "https://example.com/manifest.yaml":
			return []byte(doc), nil
		case "https://example.com/mailer":
			return binary, nil
		}
		return nil, fmt.Errorf("unexpected fetch %s", url)
	}
	res, err := Install(context.Background(), root, "https://example.com/manifest.yaml", fetch)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Digest != Digest(binary) {
		t.Fatalf("digest = %s", res.Digest)
	}
	if _, err := Resolve(root, Ref{Name: "mailer", Version: "2.0"}); err != nil {
		t.Fatalf("Resolve after URL install: %v", err)
	}
}
