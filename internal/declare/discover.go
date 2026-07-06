package declare

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// pipelinesDirName is the canonical top-level directory holding lane folders.
const pipelinesDirName = "pipelines"

// schemasDirName is the canonical top-level directory holding the schema tree.
const schemasDirName = "schemas"

// DiscoveredComposer is a lane composer found at pipelines/<lane>/iris-declare.yaml.
type DiscoveredComposer struct {
	// Lane is the lane folder name (the canonical location).
	Lane string
	// Dir is the lane folder path.
	Dir string
	// Spec is the parsed composer declaration.
	Spec *Composer
}

// DiscoveredPipeline is a pipeline declaration found at
// pipelines/<lane>/<pipeline>/, carrying the lane it sits in (its canonical
// location) alongside the validated folder.
type DiscoveredPipeline struct {
	// Lane is the lane folder the pipeline sits in.
	Lane string
	// PipelineFolder is the validated pipeline folder (declaration and script).
	PipelineFolder
}

// Workspace is the declared world discovered under a workspace root: the lane
// composers, pipeline declarations, and table schemas found at their canonical
// locations.
type Workspace struct {
	// Composers are the lane composers, one per lane folder that has one.
	Composers []DiscoveredComposer
	// Pipelines are the pipeline declarations, each with its lane and script.
	Pipelines []DiscoveredPipeline
	// Schemas are the declared tables under schemas/.
	Schemas []DiscoveredTable
}

// DiscoverWorkspace walks a workspace root and returns its declared world from
// the canonical locations (specification section 10): lane composers at
// pipelines/<lane>/iris-declare.yaml, pipeline declarations with their single
// script at pipelines/<lane>/<pipeline>/, and table schemas at
// schemas/<schema>/<table>/. An absent pipelines/ or schemas/ tree yields an
// empty result for that part rather than an error; a malformed folder within a
// present tree is rejected.
func DiscoverWorkspace(root string) (*Workspace, error) {
	ws := &Workspace{}

	pipelinesRoot := filepath.Join(root, pipelinesDirName)
	laneEntries, err := os.ReadDir(pipelinesRoot)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No pipelines/ tree: nothing to discover there.
	case err != nil:
		return nil, fmt.Errorf("declare: read pipelines directory %s: %w", pipelinesRoot, err)
	default:
		for _, le := range laneEntries {
			if isHidden(le.Name()) || !le.IsDir() {
				continue
			}
			if err := ws.discoverLane(le.Name(), filepath.Join(pipelinesRoot, le.Name())); err != nil {
				return nil, err
			}
		}
	}

	schemasRoot := filepath.Join(root, schemasDirName)
	switch _, err := os.Stat(schemasRoot); {
	case err == nil:
		tables, err := ValidateSchemaTree(schemasRoot)
		if err != nil {
			return nil, err
		}
		ws.Schemas = tables
	case errors.Is(err, fs.ErrNotExist):
		// No schemas/ tree: nothing to discover there.
	default:
		return nil, fmt.Errorf("declare: stat schemas directory %s: %w", schemasRoot, err)
	}

	return ws, nil
}

// discoverLane discovers one lane folder's composer (if any) and its pipeline
// declarations, appending them to the workspace.
func (ws *Workspace) discoverLane(lane, laneDir string) error {
	entries, err := os.ReadDir(laneDir)
	if err != nil {
		return fmt.Errorf("declare: read lane folder %s: %w", laneDir, err)
	}

	// A lane-level iris-declare.yaml is the lane composer, one above the pipeline
	// folders it sequences.
	composerPath := filepath.Join(laneDir, declFile)
	switch data, err := readFile(composerPath); {
	case err == nil:
		decl, err := ParseDeclaration(data)
		if err != nil {
			return err
		}
		if decl.Kind != KindComposer {
			return fmt.Errorf("declare: %s is a pipeline declaration, not a lane composer; a lane-level %s carries lane and order", composerPath, declFile)
		}
		ws.Composers = append(ws.Composers, DiscoveredComposer{Lane: lane, Dir: laneDir, Spec: decl.Composer})
	case errors.Is(err, fs.ErrNotExist):
		// A single-member lane needs no composer.
	default:
		return fmt.Errorf("declare: read %s: %w", composerPath, err)
	}

	// Every subfolder of the lane is a pipeline folder.
	for _, e := range entries {
		if isHidden(e.Name()) || !e.IsDir() {
			continue
		}
		pf, err := ValidatePipelineFolder(filepath.Join(laneDir, e.Name()))
		if err != nil {
			return err
		}
		ws.Pipelines = append(ws.Pipelines, DiscoveredPipeline{Lane: lane, PipelineFolder: *pf})
	}
	return nil
}
