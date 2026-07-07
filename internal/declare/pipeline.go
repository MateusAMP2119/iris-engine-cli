package declare

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// PipelineFolder is a validated pipeline folder: the folder holds
// iris-declare.yaml and exactly one script, with no internal stage structure,
// and its declaration's name matches the folder name.
type PipelineFolder struct {
	// Dir is the absolute or caller-relative folder path.
	Dir string
	// Declaration is the parsed pipeline declaration.
	Declaration *Pipeline
	// Script is the base name of the single script file beside iris-declare.yaml.
	Script string
}

// ValidatePipelineFolder validates dir as a pipeline folder and returns its
// declaration and single script. A pipeline is a folder named for the pipeline
// containing iris-declare.yaml plus exactly one script, with no internal stage
// structure (no subdirectories); the declaration must parse as a pipeline (not a
// composer) and its name must match the folder name (specification sections 1
// and 3).
func ValidatePipelineFolder(dir string) (*PipelineFolder, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("declare: read pipeline folder %s: %w", dir, err)
	}

	var scripts, subdirs []string
	hasDecl := false
	for _, e := range entries {
		name := e.Name()
		if isHidden(name) {
			continue // skip local tooling (.venv, .git, .DS_Store) at any type.
		}
		if e.IsDir() {
			subdirs = append(subdirs, name)
			continue
		}
		if name == declFile {
			hasDecl = true
			continue
		}
		scripts = append(scripts, name)
	}

	if !hasDecl {
		return nil, fmt.Errorf("declare: pipeline folder %s has no %s", dir, declFile)
	}
	if len(subdirs) > 0 {
		sort.Strings(subdirs)
		return nil, fmt.Errorf("declare: pipeline folder %s has internal subfolder(s) %v; a pipeline is one script with no internal stage structure", dir, subdirs)
	}
	switch len(scripts) {
	case 0:
		return nil, fmt.Errorf("declare: pipeline folder %s has no script beside %s; a pipeline is exactly one script", dir, declFile)
	case 1:
		// exactly one script, as required.
	default:
		sort.Strings(scripts)
		return nil, fmt.Errorf("declare: pipeline folder %s has %d scripts %v; a pipeline is exactly one script with no internal stage structure", dir, len(scripts), scripts)
	}

	data, err := readFile(filepath.Join(dir, declFile))
	if err != nil {
		return nil, fmt.Errorf("declare: read %s: %w", filepath.Join(dir, declFile), err)
	}
	decl, err := ParseDeclaration(data)
	if err != nil {
		return nil, err
	}
	if decl.Kind != KindPipeline {
		return nil, fmt.Errorf("declare: %s in %s is a lane composer, not a pipeline declaration", declFile, dir)
	}
	if base := filepath.Base(dir); decl.Pipeline.Name != base {
		return nil, fmt.Errorf("declare: pipeline name %q does not match its folder %q; the folder name is authoritative", decl.Pipeline.Name, base)
	}

	return &PipelineFolder{Dir: dir, Declaration: decl.Pipeline, Script: scripts[0]}, nil
}
