// Package declare parses and validates the declared world of an Iris workspace:
// the per-pipeline iris-declare.yaml files (pipeline declarations and lane
// composers) and the schemas/ tree of table.yaml desired-state files. It is a
// leaf package (specification section 10) with no Iris dependencies of its own:
// pure YAML-to-model parsing plus filesystem-shape validation, the layer every
// other part of the declared-world epic builds on.
//
// The parse core (ParseDeclaration, ParseTable) is pure over bytes; the shape
// validators (ValidatePipelineFolder, ValidateSchemaTree) and the discovery
// walk (DiscoverWorkspace) read the workspace tree from its canonical locations.
// Access-grant rules (reads/writes semantics), composer interlock rules, the
// YAML-to-Postgres type mapping, and the migration ledger belong to later tasks;
// this package parses those shapes faithfully so they have a model to build on.
package declare

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// declFile is the canonical filename of every declaration: a pipeline
// declaration inside a pipeline folder, or a lane composer inside a lane folder.
const declFile = "iris-declare.yaml"

// Kind discriminates a parsed iris-declare.yaml by content: a pipeline
// declaration carries run, a lane composer carries order (specification
// section 3).
type Kind int

// The declaration kinds.
const (
	// KindPipeline is a pipeline declaration (it carries run).
	KindPipeline Kind = iota
	// KindComposer is a lane composer (it carries order).
	KindComposer
)

// String names the kind, for diagnostics.
func (k Kind) String() string {
	switch k {
	case KindPipeline:
		return "pipeline"
	case KindComposer:
		return "composer"
	default:
		return "unknown"
	}
}

// Access is one reads/writes entry: a dotted schema.table name plus the exact
// fields the pipeline touches. Both are required; the granting and validation
// semantics are a later task's, this only parses the shape.
type Access struct {
	// Table is the dotted schema.table name.
	Table string `yaml:"table"`
	// Fields are the columns the pipeline reads or writes; no implicit all-columns.
	Fields []string `yaml:"fields"`
}

// StringList is a YAML scalar-or-sequence of strings: a single scalar `x` and a
// sequence `[x, y]` both decode to a StringList. It backs env_file, which the
// spec describes as one or more external KEY=VALUE files.
type StringList []string

// UnmarshalYAML decodes either a scalar string or a sequence of strings into the
// StringList, so `env_file: ./secrets.env` and `env_file: [a.env, b.env]` are
// both accepted.
func (s *StringList) UnmarshalYAML(b []byte) error {
	var list []string
	if err := yaml.Unmarshal(b, &list); err == nil {
		*s = list
		return nil
	}
	var single string
	if err := yaml.Unmarshal(b, &single); err != nil {
		return fmt.Errorf("env_file must be a file path or a list of file paths: %w", err)
	}
	*s = StringList{single}
	return nil
}

// Pipeline is a parsed pipeline declaration: the eight-field shape of
// specification section 3. name and run are required; the rest are optional.
type Pipeline struct {
	// Name is the pipeline name; required, and must match its folder.
	Name string `yaml:"name"`
	// Run is the dev-mode direct-exec argv vector; required, a plain string list.
	Run []string `yaml:"run"`
	// Env is a Compose-style map of literal or interpolated environment values.
	Env map[string]string `yaml:"env"`
	// EnvFile names one or more external KEY=VALUE files, loaded fresh each run.
	EnvFile StringList `yaml:"env_file"`
	// Lane is the pipeline's lane; omitted means its own lane.
	Lane string `yaml:"lane"`
	// Reads are the pipeline's declared read access entries.
	Reads []Access `yaml:"reads"`
	// Writes are the pipeline's declared write access entries.
	Writes []Access `yaml:"writes"`
	// DependsOn names the pipelines this one gates on (the data gate).
	DependsOn []string `yaml:"depends_on"`
}

// Composer is a parsed lane composer: a lane folder's iris-declare.yaml carrying
// the lane name and the lane's serial order (specification section 3). The
// deeper composer rules (folder agreement, 2+ interlock) belong to a later task.
type Composer struct {
	// Lane is the lane name; must match the composer's folder.
	Lane string `yaml:"lane"`
	// Order is the lane's serial walk: the member pipeline names, in order.
	Order []string `yaml:"order"`
}

// Declaration is a parsed iris-declare.yaml discriminated by content: exactly
// one of Pipeline or Composer is set, per Kind.
type Declaration struct {
	// Kind tells which of Pipeline or Composer is populated.
	Kind Kind
	// Pipeline is set when Kind is KindPipeline.
	Pipeline *Pipeline
	// Composer is set when Kind is KindComposer.
	Composer *Composer
}

// pipelineFields is the eight-field whitelist for a pipeline declaration.
var pipelineFields = map[string]bool{
	"name": true, "run": true, "env": true, "env_file": true,
	"lane": true, "reads": true, "writes": true, "depends_on": true,
}

// pipelineFieldList is the human-readable rendering of pipelineFields, in the
// spec's declaration order, for error messages.
const pipelineFieldList = "name, run, env, env_file, lane, reads, writes, depends_on"

// composerFields is the whitelist for a lane composer.
var composerFields = map[string]bool{"lane": true, "order": true}

// composerFieldList is the human-readable rendering of composerFields.
const composerFieldList = "lane, order"

// ParseDeclaration parses an iris-declare.yaml document into a Declaration,
// discriminating a pipeline (carries run) from a lane composer (carries order)
// by content. It enforces the field whitelist for the discovered shape, naming
// any offending key, and the required-field rules for a pipeline (name and run).
// A document carrying both run and order is neither shape and is rejected.
func ParseDeclaration(data []byte) (*Declaration, error) {
	raw := map[string]any{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("declare: parse %s: %w", declFile, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("declare: empty declaration: %s needs name and run (pipeline) or lane and order (composer)", declFile)
	}

	_, hasRun := raw["run"]
	_, hasOrder := raw["order"]
	switch {
	case hasRun && hasOrder:
		return nil, fmt.Errorf("declare: declaration carries both %q (pipeline) and %q (composer); a file is one or the other", "run", "order")
	case hasOrder:
		return parseComposer(raw, data)
	default:
		return parsePipeline(raw, data)
	}
}

// parsePipeline validates and decodes a pipeline-shaped declaration.
func parsePipeline(raw map[string]any, data []byte) (*Declaration, error) {
	if err := checkKeys(raw, pipelineFields, pipelineFieldList); err != nil {
		return nil, err
	}
	if _, err := requireNonEmptyString(raw, "name"); err != nil {
		return nil, err
	}
	if err := requireArgvList(raw); err != nil {
		return nil, err
	}
	var p Pipeline
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("declare: parse pipeline declaration: %w", err)
	}
	return &Declaration{Kind: KindPipeline, Pipeline: &p}, nil
}

// parseComposer validates and decodes a composer-shaped declaration. Only the
// shape is checked here (order is a string list); the lane-agreement and
// interlock rules belong to a later task.
func parseComposer(raw map[string]any, data []byte) (*Declaration, error) {
	if err := checkKeys(raw, composerFields, composerFieldList); err != nil {
		return nil, err
	}
	if err := requireStringList(raw, "order"); err != nil {
		return nil, err
	}
	var c Composer
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("declare: parse composer declaration: %w", err)
	}
	return &Declaration{Kind: KindComposer, Composer: &c}, nil
}

// checkKeys returns an error naming the first key of raw, in sorted order, that
// is not in the allowed whitelist.
func checkKeys(raw map[string]any, allowed map[string]bool, allowedList string) error {
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] {
			return fmt.Errorf("declare: unknown field %q; allowed fields are %s", k, allowedList)
		}
	}
	return nil
}

// requireNonEmptyString requires key to be present in raw as a non-blank string.
func requireNonEmptyString(raw map[string]any, key string) (string, error) {
	v, ok := raw[key]
	if !ok {
		return "", fmt.Errorf("declare: missing required field %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("declare: field %q must be a string", key)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("declare: field %q must not be empty", key)
	}
	return s, nil
}

// requireArgvList requires run to be present in raw as a non-empty list of
// strings: the argv vector, a plain string list with no shell.
func requireArgvList(raw map[string]any) error {
	v, ok := raw["run"]
	if !ok {
		return fmt.Errorf("declare: missing required field %q (the argv vector, e.g. [python, main.py])", "run")
	}
	list, ok := v.([]any)
	if !ok {
		return fmt.Errorf("declare: field %q must be a string list (argv vector), e.g. [python, main.py]", "run")
	}
	if len(list) == 0 {
		return fmt.Errorf("declare: field %q must not be empty; argv needs a program", "run")
	}
	for i, e := range list {
		if _, ok := e.(string); !ok {
			return fmt.Errorf("declare: field %q element %d is not a string; run is a plain string vector", "run", i)
		}
	}
	return nil
}

// requireStringList requires key to be present in raw as a list of strings.
func requireStringList(raw map[string]any, key string) error {
	v, ok := raw[key]
	if !ok {
		return fmt.Errorf("declare: missing required field %q", key)
	}
	list, ok := v.([]any)
	if !ok {
		return fmt.Errorf("declare: field %q must be a string list", key)
	}
	for i, e := range list {
		if _, ok := e.(string); !ok {
			return fmt.Errorf("declare: field %q element %d is not a string", key, i)
		}
	}
	return nil
}

// readFile reads a workspace file. The path is a user-workspace location by
// design -- this package parses the declared world under a caller-supplied root
// -- so the G304 file-inclusion warning is expected and accepted here.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: workspace files under a caller-supplied root are this package's input by design.
}

// isHidden reports whether a directory entry name is a hidden dotfile (e.g. a
// macOS .DS_Store), skipped by the tree walkers.
func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}
