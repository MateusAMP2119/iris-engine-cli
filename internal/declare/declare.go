// Package declare parses and validates the declared world of an Iris workspace:
// the per-pipeline iris-declare.yaml files (pipeline declarations and lane
// composers) and the schemas/ tree of table.yaml desired-state files. It is a
// leaf package with no Iris dependencies of its own: pure YAML-to-model parsing
// plus filesystem-shape validation, the layer every other part of the
// declared-world epic builds on.
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
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// declFile is the canonical filename of every declaration: a pipeline
// declaration inside a pipeline folder, or a lane composer inside a lane folder.
const declFile = "iris-declare.yaml"

// Kind discriminates a parsed iris-declare.yaml by content: a pipeline
// declaration carries run, a lane composer carries order.
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
// sequence `[x, y]` both decode to a StringList. It backs env_file: one or more
// external KEY=VALUE files.
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

// Logs is a pipeline's declared run-log recording contract: how the engine
// captures the run's stdout and stderr. Omitted, the engine default applies
// (one combined raw stream, no stamp) and the apply surfaces an advisory
// warning, so the recording contract stays visible in the declaration.
type Logs struct {
	// Split captures stdout and stderr as separately tagged streams in one
	// ordered capture, so consumers can tell error output apart or filter a
	// single stream, instead of one undifferentiated byte stream.
	Split bool `yaml:"split"`
	// Stamp frames the capture with machine-readable run metadata: a header
	// carrying the run id, pipeline, and start time, and a footer carrying the
	// end time and exit code.
	Stamp bool `yaml:"stamp"`
}

// The declared plugin lifetimes (#215); only run-lifetime tools execute today,
// lane/resident parse so stage 3 has a model waiting.
const (
	// LifetimeRun scopes the plugin to one run (tool: exec per call); the default.
	LifetimeRun = "run"
	// LifetimeLane shares one instance across a lane's serial walk (later).
	LifetimeLane = "lane"
	// LifetimeResident keeps one instance alive across runs (later).
	LifetimeResident = "resident"
)

// PluginUse is one declared plugin binding: alias → installed ref + lifetime.
type PluginUse struct {
	// Ref is the "name@version" reference to an installed plugin; required.
	Ref string `yaml:"ref"`
	// Lifetime is the declared instance lifetime; empty means run.
	Lifetime string `yaml:"lifetime"`
	// Fresh demands a cold instance for this pipeline's runs: a shared lane or
	// resident instance is never attached warm, it is replaced before the run
	// (state carried across runs stays visible in lineage; fresh opts out).
	Fresh bool `yaml:"fresh"`
}

// EffectiveLifetime returns the declared lifetime, defaulting to run.
func (p PluginUse) EffectiveLifetime() string {
	if p.Lifetime == "" {
		return LifetimeRun
	}
	return p.Lifetime
}

// Pipeline is a parsed pipeline declaration: the ten-field declaration shape.
// name and run are required; the rest are optional.
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
	// Logs is the pipeline's run-log recording contract; nil means the engine
	// default (combined raw stream, no stamp), surfaced as an apply warning.
	Logs *Logs `yaml:"logs"`
	// Plugins are the pipeline's declared plugin bindings by alias: the only
	// external capabilities its runs may call, each digest-pinned at run start.
	Plugins map[string]PluginUse `yaml:"plugins"`
	// Reads are the pipeline's declared read access entries.
	Reads []Access `yaml:"reads"`
	// Writes are the pipeline's declared write access entries.
	Writes []Access `yaml:"writes"`
	// DependsOn names the pipelines this one gates on (the data gate).
	DependsOn []string `yaml:"depends_on"`
}

// Composer is a parsed lane composer: the lane name, its serial order, and the optional folder surface (surface.go owns its rules).
type Composer struct {
	// Lane is the lane name; must match the composer's folder.
	Lane string `yaml:"lane"`
	// Order is the lane's serial walk: the member pipeline names, in order.
	Order []string `yaml:"order"`
	// Reads is the folder surface's read side; absent surface leaves members unconstrained.
	Reads []Access `yaml:"reads"`
	// Writes is the folder surface's write side.
	Writes []Access `yaml:"writes"`
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

// pipelineFields is the ten-field whitelist for a pipeline declaration.
var pipelineFields = map[string]bool{
	"name": true, "run": true, "env": true, "env_file": true, "lane": true,
	"logs": true, "plugins": true, "reads": true, "writes": true, "depends_on": true,
}

// pipelineFieldList is the human-readable rendering of pipelineFields, in
// declaration order, for error messages.
const pipelineFieldList = "name, run, env, env_file, lane, logs, plugins, reads, writes, depends_on"

// logsFields is the whitelist for the logs block inside a pipeline declaration.
var logsFields = map[string]bool{"split": true, "stamp": true}

// logsFieldList is the human-readable rendering of logsFields.
const logsFieldList = "split, stamp"

// composerFields is the whitelist for a lane composer.
var composerFields = map[string]bool{"lane": true, "order": true, "reads": true, "writes": true}

// composerFieldList is the human-readable rendering of composerFields.
const composerFieldList = "lane, order, reads, writes"

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
	if err := checkLogsShape(raw); err != nil {
		return nil, err
	}
	if err := checkPluginsShape(raw); err != nil {
		return nil, err
	}
	var p Pipeline
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("declare: parse pipeline declaration: %w", err)
	}
	return &Declaration{Kind: KindPipeline, Pipeline: &p}, nil
}

// checkLogsShape validates an optional logs block: a mapping whose keys are the
// logs whitelist and whose values are booleans. An absent block is valid (the
// engine default applies); any other shape names the offending key or value.
func checkLogsShape(raw map[string]any) error {
	v, ok := raw["logs"]
	if !ok {
		return nil
	}
	block, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("declare: field %q must be a mapping of %s", "logs", logsFieldList)
	}
	if err := checkKeys(block, logsFields, logsFieldList); err != nil {
		return err
	}
	for _, key := range []string{"split", "stamp"} {
		bv, present := block[key]
		if !present {
			continue
		}
		if _, isBool := bv.(bool); !isBool {
			return fmt.Errorf("declare: logs field %q must be a boolean", key)
		}
	}
	return nil
}

// pluginsFields is the whitelist for one plugin binding inside the plugins block.
var pluginsFields = map[string]bool{"ref": true, "lifetime": true, "fresh": true}

// pluginsFieldList is the human-readable rendering of pluginsFields.
const pluginsFieldList = "ref, lifetime, fresh"

// pluginAliasRe admits plugin aliases: dot-free slugs (the alias prefixes call verbs as "alias.verb").
var pluginAliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// checkPluginsShape validates an optional plugins block: alias slug → {ref
// name@version, lifetime run|lane|resident}. Install/digest checks are the
// engine's at run start; this is shape only.
func checkPluginsShape(raw map[string]any) error {
	v, ok := raw["plugins"]
	if !ok {
		return nil
	}
	block, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("declare: field %q must be a mapping of alias to {%s}", "plugins", pluginsFieldList)
	}
	aliases := make([]string, 0, len(block))
	for alias := range block {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		if !pluginAliasRe.MatchString(alias) {
			return fmt.Errorf("declare: plugin alias %q is not a lowercase slug ([a-z0-9_-])", alias)
		}
		entry, ok := block[alias].(map[string]any)
		if !ok {
			return fmt.Errorf("declare: plugin %q must be a mapping of %s", alias, pluginsFieldList)
		}
		if err := checkKeys(entry, pluginsFields, pluginsFieldList); err != nil {
			return err
		}
		ref, ok := entry["ref"].(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return fmt.Errorf("declare: plugin %q needs a non-empty %q (name@version)", alias, "ref")
		}
		name, version, cut := strings.Cut(ref, "@")
		if !cut || strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" {
			return fmt.Errorf("declare: plugin %q ref %q is not name@version", alias, ref)
		}
		if lv, present := entry["lifetime"]; present {
			s, ok := lv.(string)
			if !ok || (s != LifetimeRun && s != LifetimeLane && s != LifetimeResident) {
				return fmt.Errorf("declare: plugin %q lifetime must be %s, %s, or %s", alias, LifetimeRun, LifetimeLane, LifetimeResident)
			}
		}
		if fv, present := entry["fresh"]; present {
			if _, ok := fv.(bool); !ok {
				return fmt.Errorf("declare: plugin %q fresh must be a boolean", alias)
			}
		}
	}
	return nil
}

// parseComposer validates and decodes a composer-shaped declaration. Only the
// shape is checked here (order is a string list); the lane-agreement and
// interlock rules belong to a later task.
func parseComposer(raw map[string]any, data []byte) (*Declaration, error) {
	if err := checkKeys(raw, composerFields, composerFieldList); err != nil {
		return nil, err
	}
	if _, err := requireNonEmptyString(raw, "lane"); err != nil {
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
	if len(list) == 0 {
		return fmt.Errorf("declare: field %q must not be empty", key)
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
