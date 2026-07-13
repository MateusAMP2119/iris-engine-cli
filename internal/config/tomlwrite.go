package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// UpsertTOMLFile records the given string settings in the iris.toml at path,
// creating the file (and its directory) when absent. Every line it does not own
// is preserved verbatim -- comments, blank lines, and keys outside set -- so a
// hand-edited file survives a re-connect. A key already present is rewritten in
// place (any duplicate of it later in the file is dropped, since the parser is
// last-one-wins); a missing key is appended. The file is written 0600: it may
// carry a token, and the engine settings it holds are operator-private either
// way.
func UpsertTOMLFile(path string, set map[string]string) error {
	for key := range set {
		if !isBareKey(key) {
			return fmt.Errorf("config: upsert iris.toml: malformed key %q (flat identifiers only)", key)
		}
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the resolved iris.toml location the CLI computes from the workspace, not attacker-controlled network input.
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("config: read %s: %w", path, err)
	}

	written := map[string]bool{}
	var out []string
	for _, raw := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if len(data) == 0 {
			break // an absent or empty file contributes no lines
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			out = append(out, raw)
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			out = append(out, raw)
			continue
		}
		key = strings.TrimSpace(key)
		if _, owned := set[key]; !owned {
			out = append(out, raw)
			continue
		}
		if written[key] {
			continue // drop a duplicate: the parser is last-one-wins, so a kept one would override the rewrite
		}
		out = append(out, key+" = "+quoteTOML(set[key]))
		written[key] = true
	}
	missing := make([]string, 0, len(set))
	for key := range set {
		if !written[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		out = append(out, key+" = "+quoteTOML(set[key]))
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config: create %s: %w", filepath.Dir(path), err)
	}
	content := strings.Join(out, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec // G703: path is the resolved iris.toml location the CLI computes from the workspace, not attacker-controlled network input.
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	// WriteFile applies the mode only on create: tighten a pre-existing file too,
	// since this write may be the one that adds a token to it.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("config: chmod %s: %w", path, err)
	}
	return nil
}

// quoteTOML renders a value as the double-quoted string the parser reads back,
// escaping the two escapes it recognizes (backslash and double quote).
func quoteTOML(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
}
