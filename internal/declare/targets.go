package declare

import (
	"fmt"
	"os"
	"path/filepath"
)

// ResolveDeclarationFile resolves a user-supplied apply/destroy target to
// exactly one iris-declare.yaml, per specification sections 3, 6.3, and 8:
// apply and destroy each target exactly one declaration file, never a
// workspace or a set. A file path must be named iris-declare.yaml; a folder
// path resolves to <folder>/iris-declare.yaml, with no further search -- a
// folder that itself has no declaration is a precise error naming it, never a
// sweep into subfolders (no workspace-wide discovery, no transitive
// chaining). It does not parse the file; use LoadDeclarationFile for that.
func ResolveDeclarationFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("declare: resolve declaration target %s: %w", path, err)
	}
	if !info.IsDir() {
		if filepath.Base(path) != declFile {
			return "", fmt.Errorf("declare: %s is not named %s; iris declare apply/destroy target exactly one declaration file", path, declFile)
		}
		return path, nil
	}
	candidate := filepath.Join(path, declFile)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("declare: folder %s has no %s; apply/destroy target exactly one declaration file, never a workspace sweep", path, declFile)
	}
	return candidate, nil
}

// LoadDeclarationFile resolves path per ResolveDeclarationFile, then reads and
// parses the resolved file. It returns the resolved file path alongside the
// parsed Declaration, so a caller that supplied a folder learns the file it
// actually targeted.
func LoadDeclarationFile(path string) (resolved string, decl *Declaration, err error) {
	resolved, err = ResolveDeclarationFile(path)
	if err != nil {
		return "", nil, err
	}
	data, err := readFile(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("declare: read %s: %w", resolved, err)
	}
	decl, err = ParseDeclaration(data)
	if err != nil {
		return "", nil, err
	}
	return resolved, decl, nil
}
