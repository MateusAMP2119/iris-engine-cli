package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// This file loads the folder-surface context a declare apply validates against (#192): the lane folder's composer (with its optional reads/writes surface) and the sibling pipeline declarations on disk.

// folderContext reads the composer and sibling declarations around a pipeline folder; a lane folder without a composer file returns (nil, nil, nil) and surface rules stay off.
func folderContext(pipelineDir string) (*declare.Composer, []*declare.Pipeline, error) {
	laneDir := filepath.Dir(pipelineDir)
	comp, err := loadComposerFile(filepath.Join(laneDir, "iris-declare.yaml"))
	if err != nil || comp == nil {
		return nil, nil, err
	}
	own := filepath.Base(pipelineDir)
	siblings, err := loadSiblingPipelines(laneDir, own)
	if err != nil {
		return nil, nil, err
	}
	return comp, siblings, nil
}

// loadComposerFile parses path as a composer declaration; an absent file or a non-composer document is (nil, nil).
func loadComposerFile(path string) (*declare.Composer, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: an engine-registered lane folder under the leader's own workspace.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("declare apply: read composer %s: %w", path, err)
	}
	d, err := declare.ParseDeclaration(raw)
	if err != nil {
		return nil, fmt.Errorf("declare apply: parse composer %s: %w", path, err)
	}
	if d.Kind != declare.KindComposer {
		return nil, nil
	}
	return d.Composer, nil
}

// loadSiblingPipelines parses every sibling pipeline declaration in laneDir (skipping the folder named own); a broken sibling refuses, since exclusivity cannot be validated over an unreadable claim.
func loadSiblingPipelines(laneDir, own string) ([]*declare.Pipeline, error) {
	entries, err := os.ReadDir(laneDir)
	if err != nil {
		return nil, fmt.Errorf("declare apply: read lane folder %s: %w", laneDir, err)
	}
	var siblings []*declare.Pipeline
	for _, e := range entries {
		if !e.IsDir() || e.Name() == own || e.Name()[0] == '.' {
			continue
		}
		path := filepath.Join(laneDir, e.Name(), "iris-declare.yaml")
		raw, err := os.ReadFile(path) //nolint:gosec // G304: sibling pipeline folders under the leader's own workspace.
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("declare apply: read sibling declaration %s: %w", path, err)
		}
		d, err := declare.ParseDeclaration(raw)
		if err != nil {
			return nil, fmt.Errorf("declare apply: parse sibling declaration %s: %w", path, err)
		}
		if d.Kind == declare.KindPipeline {
			siblings = append(siblings, d.Pipeline)
		}
	}
	return siblings, nil
}

// composerMembers parses the order-named member declarations under the composer's folder; members without a folder or declaration on disk are skipped (their own apply validates them).
func composerMembers(laneDir string, order []string) (map[string]*declare.Pipeline, error) {
	members := map[string]*declare.Pipeline{}
	for _, name := range order {
		if name != filepath.Base(name) || name == "." || name == ".." {
			return nil, fmt.Errorf("declare apply: composer order entry %q is not a plain folder name", name)
		}
		path := filepath.Join(laneDir, name, "iris-declare.yaml")
		raw, err := os.ReadFile(path) //nolint:gosec // G304: member pipeline folders under the leader's own workspace.
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("declare apply: read member declaration %s: %w", path, err)
		}
		d, err := declare.ParseDeclaration(raw)
		if err != nil {
			return nil, fmt.Errorf("declare apply: parse member declaration %s: %w", path, err)
		}
		if d.Kind == declare.KindPipeline {
			members[name] = d.Pipeline
		}
	}
	return members, nil
}
