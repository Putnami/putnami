package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ProjectConfig is the project-level putnami.json schema.
type ProjectConfig struct {
	Schema       string         `json:"$schema,omitempty"`
	Name         string         `json:"name,omitempty"`
	Type         string         `json:"type,omitempty"`
	Description  string         `json:"description,omitempty"`
	Main         string         `json:"main,omitempty"`
	Bin          *BinConfig     `json:"bin,omitempty"`
	Exports      *ExportsConfig `json:"exports,omitempty"`
	Tags         []string       `json:"tags,omitempty"`
	Publish      []string       `json:"publish,omitempty"`
	RunsWith     []string       `json:"runsWith,omitempty"`
	Build        *BuildConfig   `json:"build,omitempty"`
	Dependencies []string       `json:"dependencies,omitempty"`
	Extensions   []string       `json:"extensions,omitempty"`
	// Registries overrides the workspace registry entry of an ecosystem for
	// this project. The override replaces the entry whole, for the same reason
	// the workspace merge does: the entry's shape belongs to the ecosystem
	// profile, not to this module.
	Registries map[string]json.RawMessage `json:"registries,omitempty"`
	// Visibility is this project's import boundary. Empty means the default,
	// DefaultVisibility.
	Visibility Visibility `json:"visibility,omitempty"`
	// Distribution is who may pull what this project publishes. It is not
	// Visibility above, which is an import boundary; see DistributionConfig.
	Distribution *DistributionConfig       `json:"distribution,omitempty"`
	Options      map[string]map[string]any `json:"options,omitempty"`
	// Jobs is ACCEPTED BUT IGNORED. It is kept parseable only so its presence is
	// detectable: the loader warns when a project declares it (see the CLI's
	// workspace loader) and the strict validator reports it, so an authored
	// `jobs` block is never a silent no-op.
	//
	// It is deliberately opaque. The pre-v3 shape this used to model — a flat
	// {kind, command, args, …} override — cannot be honored: today's jobs are
	// steps→tasks that declare their Reads/Writes, carry a cache policy, and fold
	// a contract digest into the cache key. A raw command/args override would
	// punch a contract-free execution path straight through that key. Per-project
	// execution tuning belongs on `tasks` (ProjectTaskTuning); opting out of an
	// extension-provided job belongs on `disable.jobs`.
	Jobs    map[string]json.RawMessage   `json:"jobs,omitempty"`
	Tasks   map[string]ProjectTaskTuning `json:"tasks,omitempty"`
	Disable *ProjectDisableConfig        `json:"disable,omitempty"`
	// FeatureAuthority records why a published project declares no feature of
	// its own. Absent means the ordinary case: the project is expected to
	// declare one, and completeness tooling says so when it does not.
	FeatureAuthority *ProjectFeatureAuthority `json:"featureAuthority,omitempty"`
}

// ProjectFeatureAuthority answers "where is this project's user-facing feature"
// for a published project that declares none, in the project that owns the
// answer. Exactly one member is set.
//
// Both answers already existed as prose in module READMEs, which no check can
// read: a protocol module explaining that a wire contract is not a feature a
// user enables, and go.putnami.dev/{openapi,proto} explaining that they serve
// an outcome declared by go.putnami.dev/api. Tooling could only see "no feature
// here" and report a gap that review had already closed.
//
// This is deliberately not a suppression switch. `None` must carry its reason,
// and an `Owner` naming no authored feature is a louder failure than the
// missing link it claims to explain — otherwise the field would become the
// cheapest way to silence the completeness check.
type ProjectFeatureAuthority struct {
	// Owner is the authored feature ID this project contributes to when the
	// declaration lives in another project.
	Owner string `json:"owner,omitempty"`
	// None states, in one sentence, why this project deliberately has no
	// user-facing feature. Prose is the point: the reason is what a reviewer
	// checks, and a bare boolean would carry none.
	None string `json:"none,omitempty"`
}

// BinConfig models the `bin` field, whose schema is
// oneOf: [ string, object<string,string> ]. Exactly one of String or Map is
// populated per the wire form, so re-serialization is byte-identical to the
// input form (string stays a string, object stays an object). Modeling it as a
// typed union (rather than `any`) lets DisallowUnknownFields reject a wrongly
// typed value — e.g. a nested object or a number — instead of silently passing.
type BinConfig struct {
	// String is set when `bin` is a JSON string.
	String string
	// Map is set (non-nil) when `bin` is a JSON object of name -> path.
	Map map[string]string
}

// UnmarshalJSON accepts either a JSON string or a JSON object of string values.
// Any other shape (number, array, boolean, or an object whose values are not
// strings) is rejected, so the strict contract now covers the `bin` subtree.
func (b *BinConfig) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		b.String = s
		b.Map = nil
		return nil
	case '{':
		m := make(map[string]string)
		if err := strictUnmarshal(trimmed, &m); err != nil {
			return fmt.Errorf("bin object: %w", err)
		}
		b.Map = m
		b.String = ""
		return nil
	default:
		return fmt.Errorf("bin must be a string or an object of string values")
	}
}

// MarshalJSON re-emits the union in its canonical form: the string form when the
// input was a string, otherwise the object form (with map keys sorted by
// encoding/json). This keeps describe/generate output stable across runs.
func (b BinConfig) MarshalJSON() ([]byte, error) {
	if b.Map != nil {
		return json.Marshal(b.Map)
	}
	return json.Marshal(b.String)
}

// ExportsConfig models the `exports` field, whose schema is
// oneOf: [ string, object<string, ExportEntry> ] where each ExportEntry is
// itself oneOf: [ string, conditional-object ]. Exactly one of String or Map is
// populated per the wire form so re-serialization preserves it.
type ExportsConfig struct {
	// String is set when `exports` is a JSON string.
	String string
	// Map is set (non-nil) when `exports` is a JSON object of subpath -> entry.
	Map map[string]ExportEntry
}

// ExportEntry is one value in the `exports` object: either a plain string target
// or a conditional-export object with a closed set of keys. Exactly one of
// String or Conditions is populated per the wire form.
type ExportEntry struct {
	// String is set when the entry is a JSON string.
	String string
	// Conditions is set (non-nil) when the entry is a conditional-export object.
	Conditions *ExportConditions
}

// ExportConditions is the closed-key conditional-export object. The schema
// declares additionalProperties: false, so unknown condition keys are rejected.
type ExportConditions struct {
	Types   string `json:"types,omitempty"`
	Import  string `json:"import,omitempty"`
	Require string `json:"require,omitempty"`
	Default string `json:"default,omitempty"`
}

// UnmarshalJSON accepts either a JSON string or a JSON object of ExportEntry
// values. Any other top-level shape is rejected.
func (e *ExportsConfig) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		e.String = s
		e.Map = nil
		return nil
	case '{':
		// Entries carry their own strict UnmarshalJSON, so unknown keys inside a
		// conditional-export object are rejected below.
		m := make(map[string]ExportEntry)
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return fmt.Errorf("exports object: %w", err)
		}
		e.Map = m
		e.String = ""
		return nil
	default:
		return fmt.Errorf("exports must be a string or an object")
	}
}

// MarshalJSON re-emits the union in its canonical form: string form when the
// input was a string, otherwise the object form (map keys sorted by
// encoding/json).
func (e ExportsConfig) MarshalJSON() ([]byte, error) {
	if e.Map != nil {
		return json.Marshal(e.Map)
	}
	return json.Marshal(e.String)
}

// UnmarshalJSON accepts either a JSON string or a closed-key conditional-export
// object. An unknown key inside the object (e.g. "browser") is rejected.
func (e *ExportEntry) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		e.String = s
		e.Conditions = nil
		return nil
	case '{':
		var cond ExportConditions
		if err := strictUnmarshal(trimmed, &cond); err != nil {
			return fmt.Errorf("export conditions: %w", err)
		}
		e.Conditions = &cond
		e.String = ""
		return nil
	default:
		return fmt.Errorf("export entry must be a string or a conditional-export object")
	}
}

// MarshalJSON re-emits the entry in its canonical form.
func (e ExportEntry) MarshalJSON() ([]byte, error) {
	if e.Conditions != nil {
		return json.Marshal(e.Conditions)
	}
	return json.Marshal(e.String)
}

// strictUnmarshal decodes JSON into v with unknown fields rejected. It is used
// by the bin/exports unions so the project-config strict contract reaches into
// their nested objects (which custom UnmarshalJSON methods otherwise bypass).
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ProjectTaskTuning carries per-project execution tuning for a scheduled task.
// Keys in ProjectConfig.Tasks are job names: either a full step name
// ("build~transpile") or a command name ("lint"), which applies to every step
// of that command unless a more specific step entry exists.
type ProjectTaskTuning struct {
	// CPUWeight is a relative multiplier on the task's deterministic,
	// history-derived CPU demand. 1 is the default; 4 gives an otherwise
	// identical task four times the CPU budget, capped by machine capacity. It
	// is an execution hint only and never affects cache keys.
	CPUWeight *float64 `json:"cpuWeight,omitempty"`

	// TimeoutMs is the scheduler deadline, in milliseconds, for this project's
	// copy of the task. It is an execution hint only and never affects cache
	// keys. A positive project value overrides the extension manifest's timeout;
	// the manifest's zero (default) and negative (unbounded) sentinels are not
	// selectable from project config.
	TimeoutMs *int `json:"timeoutMs,omitempty"`
}

// BuildConfig defines build-time configuration for a project.
type BuildConfig struct {
	Assets  []BuildAsset      `json:"assets,omitempty"`
	Compile map[string]string `json:"compile,omitempty"`
}

// BuildAsset defines a build asset to copy.
type BuildAsset struct {
	From string `json:"from,omitempty"`
}

// ProjectDisableConfig controls project-level disabling of jobs.
type ProjectDisableConfig struct {
	Jobs []string `json:"jobs,omitempty"`
}

// LoadProjectConfig reads a project-level config file (putnami.json).
//
// It is the lenient loader used by existing callers: unreadable and malformed
// files return nil, and diagnostics are discarded. Workspace discovery uses
// LoadProjectConfigWithDiagnostics so new task-timeout validation cannot be
// silently bypassed on the production load path.
func LoadProjectConfig(projectDir string) *ProjectConfig {
	c, diags := LoadProjectConfigWithDiagnostics(projectDir)
	if diag.HasErrors(diags) {
		return nil
	}
	return c
}

// LoadProjectConfigWithDiagnostics returns a leniently decoded project config
// plus validation findings for per-task execution tuning. It intentionally does
// not run the whole strict project validator: that would make pre-existing,
// partially-authored configs fail workspace discovery. The `tasks` block alone
// is decoded strictly so a typo or malformed timeout cannot silently fall back
// to the manifest deadline.
func LoadProjectConfigWithDiagnostics(projectDir string) (*ProjectConfig, []diag.Diagnostic) {
	path := ResolveFile(projectDir, ConfigFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}

	// First retain the raw task block. Decoding the full ProjectConfig before
	// this would make any type error inside tasks indistinguishable from the
	// historically lenient errors in unrelated legacy fields.
	var raw struct {
		Tasks json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil
	}
	tasks, taskDiags := decodeProjectTaskTuning(raw.Tasks)
	if diag.HasErrors(taskDiags) {
		return nil, taskDiags
	}

	var c ProjectConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, nil
	}
	c.Tasks = tasks
	return &c, ValidateProjectTaskTuning(&c)
}

// decodeProjectTaskTuning decodes the new execution-tuning surface strictly
// while the rest of putnami.json remains compatible with the legacy lenient
// loader. Exact field names are checked before decoding because encoding/json
// otherwise accepts case-insensitive matches such as "timeoutMS".
func decodeProjectTaskTuning(raw json.RawMessage) (map[string]ProjectTaskTuning, []diag.Diagnostic) {
	if raw == nil {
		return nil, nil
	}

	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		if err == nil {
			err = fmt.Errorf("task tuning must be an object")
		}
		return nil, []diag.Diagnostic{
			diag.Errorf("invalid-task-tuning", "tasks", "failed to parse task tuning: %v", err),
		}
	}

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	tunings := make(map[string]ProjectTaskTuning, len(entries))
	var diags []diag.Diagnostic
	for _, name := range names {
		tuning, entryDiags := decodeProjectTaskTuningEntry(name, entries[name])
		diags = append(diags, entryDiags...)
		if len(entryDiags) == 0 {
			tunings[name] = tuning
		}
	}
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return tunings, nil
}

func decodeProjectTaskTuningEntry(name string, raw json.RawMessage) (ProjectTaskTuning, []diag.Diagnostic) {
	field := fmt.Sprintf("tasks.%s", name)
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(raw, &properties); err != nil || properties == nil {
		if err == nil {
			err = fmt.Errorf("task tuning must be an object")
		}
		return ProjectTaskTuning{}, []diag.Diagnostic{
			diag.Errorf("invalid-task-tuning", field, "failed to parse task tuning: %v", err),
		}
	}

	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "cpuWeight" || key == "timeoutMs" {
			continue
		}
		return ProjectTaskTuning{}, []diag.Diagnostic{
			diag.Errorf("unknown-task-tuning-field", field+"."+key, "unknown task tuning field %q", key),
		}
	}

	var tuning ProjectTaskTuning
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tuning); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			field += "." + typeErr.Field
		}
		return ProjectTaskTuning{}, []diag.Diagnostic{
			diag.Errorf("invalid-task-tuning", field, "failed to parse task tuning: %v", err),
		}
	}
	return tuning, nil
}
