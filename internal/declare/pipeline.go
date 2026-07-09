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

	var files, subdirs []string
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
		files = append(files, name)
	}

	if !hasDecl {
		return nil, fmt.Errorf("declare: pipeline folder %s has no %s", dir, declFile)
	}
	if len(subdirs) > 0 {
		sort.Strings(subdirs)
		return nil, fmt.Errorf("declare: pipeline folder %s has internal subfolder(s) %v; a pipeline is one script with no internal stage structure", dir, subdirs)
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

	// Files named by the declaration's env_file are run-time secret inputs (read
	// fresh each run, never stored), not the pipeline script: only the run: argv
	// names the script. Exclude any that sit directly in the folder before the
	// single-script count, so the golden sample's env_file demo (specification
	// section 3) validates as one script beside its secrets file.
	envInputs := envFileNames(decl.Pipeline.EnvFile)
	var scripts []string
	for _, name := range files {
		if envInputs[name] {
			continue
		}
		scripts = append(scripts, name)
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

	if base := filepath.Base(dir); decl.Pipeline.Name != base {
		return nil, fmt.Errorf("declare: pipeline name %q does not match its folder %q; the folder name is authoritative", decl.Pipeline.Name, base)
	}

	return &PipelineFolder{Dir: dir, Declaration: decl.Pipeline, Script: scripts[0]}, nil
}

// envFileNames returns the set of base file names referenced by env_file entries
// that live directly in the pipeline folder (a bare name after cleaning, e.g.
// ./secrets.env). Entries pointing into subfolders or outside the folder are not
// candidate scripts anyway and are ignored here.
func envFileNames(entries StringList) map[string]bool {
	names := map[string]bool{}
	for _, ef := range entries {
		clean := filepath.Clean(ef)
		if clean == "." || clean == ".." {
			continue
		}
		if filepath.Base(clean) == clean {
			names[clean] = true
		}
	}
	return names
}
