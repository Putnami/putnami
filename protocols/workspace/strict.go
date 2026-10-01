package workspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ParseWorkspaceConfig decodes JSON data into a Config using strict mode.
// Unknown fields are rejected and structured diagnostics are returned.
func ParseWorkspaceConfig(data []byte) (*Config, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse workspace config: %v", err),
		}
	}
	return &c, nil
}

// ValidateWorkspaceConfig checks structural invariants on a parsed workspace config.
func ValidateWorkspaceConfig(c *Config) []diag.Diagnostic {
	if c == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-config", "", "workspace config is nil"),
		}
	}

	var diags []diag.Diagnostic

	if c.Name == "" {
		diags = append(diags, diag.Warningf("missing-name", "name", "workspace name is recommended"))
	}

	diags = append(diags, validateRegistries(c.Registries)...)

	// Validate hook phases reference valid commands in sorted order for deterministic diagnostics.
	if c.Hooks != nil && c.Hooks.Commands != nil {
		hookCmds := sortedHookKeys(c.Hooks.Commands)
		for _, cmd := range hookCmds {
			phase := c.Hooks.Commands[cmd]
			if phase == nil {
				diags = append(diags, diag.Warningf("nil-hook-phase", fmt.Sprintf("hooks.commands.%s", cmd), "hook phase is nil"))
			}
		}
	}

	return diags
}

// NormalizeWorkspaceConfig applies canonical defaults to a parsed workspace config
// for deterministic output. It modifies the config in place.
func NormalizeWorkspaceConfig(c *Config) {
	if c == nil {
		return
	}

	if c.Options == nil {
		c.Options = make(map[string]map[string]any)
	}
	if c.Extensions.List == nil {
		c.Extensions.List = make(map[string]string)
	}
	if c.Aliases == nil {
		c.Aliases = make(map[string]string)
	}
	if c.ProjectAliases == nil {
		c.ProjectAliases = make(map[string]string)
	}
	if c.Groups == nil {
		c.Groups = make(map[string]string)
	}
}

// ParseAndValidateWorkspaceConfig combines strict parsing and validation.
func ParseAndValidateWorkspaceConfig(data []byte) (*Config, []diag.Diagnostic) {
	c, diags := ParseWorkspaceConfig(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	vDiags := ValidateWorkspaceConfig(c)
	diags = append(diags, vDiags...)

	NormalizeWorkspaceConfig(c)

	return c, diags
}

// ParseProjectConfig decodes JSON data into a ProjectConfig using strict mode.
// Unknown fields are rejected and structured diagnostics are returned.
func ParseProjectConfig(data []byte) (*ProjectConfig, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var c ProjectConfig
	if err := dec.Decode(&c); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse project config: %v", err),
		}
	}
	return &c, nil
}

// ValidateProjectConfig checks structural invariants on a parsed project config.
func ValidateProjectConfig(c *ProjectConfig) []diag.Diagnostic {
	if c == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-config", "", "project config is nil"),
		}
	}

	var diags []diag.Diagnostic

	if c.Name == "" {
		diags = append(diags, diag.Errorf("required-field", "name", "project name is required"))
	}

	// `jobs` is accepted but never planned, so it is reported rather than
	// validated: demanding `kind`/`command` on it would strictly police a surface
	// that does nothing, and rejecting it outright would break configs that
	// already parse today. The report is warning-level so a clean validation is
	// still not a silent no-op. Reported in sorted order so two runs over the
	// same config emit identical diagnostics.
	//
	// The key is reported on PRESENCE, not on size. `{"jobs": {}}` declares the
	// ignored surface too — it is enough to mark a directory as a project — so an
	// empty block gets a field-level report rather than passing validation
	// silently. Only a nil map (absent, or `"jobs": null`) is quiet.
	const ignoredJobsAdvice = "project-level `jobs` is not implemented and never reaches planning — " +
		"use `disable.jobs` to opt out of an extension-provided job, or `tasks` for per-project execution tuning"
	if c.Jobs != nil && len(c.Jobs) == 0 {
		diags = append(diags, diag.Warningf("ignored-field", "jobs",
			"`jobs` is declared but empty, and is ignored either way: "+ignoredJobsAdvice))
	}
	for _, name := range sortedRawKeys(c.Jobs) {
		diags = append(diags, diag.Warningf("ignored-field", fmt.Sprintf("jobs.%s", name),
			"job %q is ignored: "+ignoredJobsAdvice, name))
	}

	// An unknown visibility is an error rather than a silent fallback: the
	// value decides who may import this project, and resolving a typo to the
	// default would answer a question nobody asked.
	if c.Visibility != "" && !c.Visibility.Valid() {
		diags = append(diags, diag.Errorf("invalid-visibility", "visibility",
			"unknown visibility %q (want %q or %q)", c.Visibility, VisibilityScope, VisibilityPublic))
	}

	diags = append(diags, validateDistribution(c.Distribution)...)
	diags = append(diags, validateRegistries(c.Registries)...)
	diags = append(diags, validateProjectFeatureAuthority(c)...)
	diags = append(diags, ValidateProjectTaskTuning(c)...)

	return diags
}

// EcosystemIDPattern is the alphabet of a registries key. It is the same
// ecosystem id an extension's profile declares, so the two documents key on one
// vocabulary and a typo is a validation error rather than a silently ignored
// entry.
const EcosystemIDPattern = `^[a-z][a-z0-9-]{0,31}$`

var ecosystemID = regexp.MustCompile(EcosystemIDPattern)

// validateRegistries checks the outer map of a `registries` section: the key is
// an ecosystem id and the value is a JSON object. What is INSIDE the object is
// deliberately not checked here — that shape belongs to the ecosystem profile
// the owning extension declares, and re-stating it in this module would create
// a second authority for it.
//
// Diagnostics are emitted in sorted key order so one document produces one
// report, run after run.
func validateRegistries(registries map[string]json.RawMessage) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for _, ecosystem := range sortedRawKeys(registries) {
		field := fmt.Sprintf("registries.%s", ecosystem)
		if !ecosystemID.MatchString(ecosystem) {
			diags = append(diags, diag.Errorf("invalid-registries", field,
				"registries key %q is not an ecosystem id matching %s", ecosystem, EcosystemIDPattern))
		}
		if trimmed := bytes.TrimSpace(registries[ecosystem]); len(trimmed) == 0 || trimmed[0] != '{' {
			diags = append(diags, diag.Errorf("invalid-registries", field,
				"registries entry %q must be a JSON object; its shape is owned by that ecosystem's profile", ecosystem))
		}
	}
	return diags
}

// validateProjectFeatureAuthority keeps `featureAuthority` an answer rather than
// an off switch. A declared block must say exactly one thing, and it must say
// something: an empty object, both members, or a blank reason would all silence
// the completeness check while explaining nothing.
func validateProjectFeatureAuthority(c *ProjectConfig) []diag.Diagnostic {
	authority := c.FeatureAuthority
	if authority == nil {
		return nil
	}
	owner := strings.TrimSpace(authority.Owner)
	none := strings.TrimSpace(authority.None)
	switch {
	case owner != "" && none != "":
		return []diag.Diagnostic{diag.Errorf("conflicting-field", "featureAuthority",
			"featureAuthority declares both an owning feature and no feature; set exactly one")}
	case owner == "" && none == "":
		return []diag.Diagnostic{diag.Errorf("required-field", "featureAuthority",
			"featureAuthority must set either `owner` (the feature this project contributes to) or `none` (why it has none)")}
	}
	return nil
}

// ValidateProjectTaskTuning validates the execution-only `tasks` block.
//
// timeoutMs is the one project tuning that can disarm a scheduler protection:
// extension manifests use zero for the CLI default and a negative value for no
// deadline, but a repository must not select either sentinel. Positive values
// remain intentionally unconstrained here; every deadline consumer saturates
// its duration arithmetic rather than turning a large valid value into an
// accidental unbounded timeout.
//
// This is separate from ValidateProjectConfig because workspace discovery must
// enforce it without suddenly applying the older whole-document rules to every
// existing putnami.json it scans.
func ValidateProjectTaskTuning(c *ProjectConfig) []diag.Diagnostic {
	if c == nil {
		return nil
	}

	var diags []diag.Diagnostic
	for _, name := range sortedTaskKeys(c.Tasks) {
		tuning := c.Tasks[name]
		if tuning.TimeoutMs == nil || *tuning.TimeoutMs > 0 {
			continue
		}
		diags = append(diags, diag.Errorf(
			"invalid-timeout",
			fmt.Sprintf("tasks.%s.timeoutMs", name),
			"task timeoutMs must be positive; 0 (use the CLI default) and negative values (no deadline) are extension-manifest sentinels",
		))
	}
	return diags
}

// NormalizeProjectConfig applies canonical defaults to a parsed project config
// for deterministic output. It modifies the config in place.
func NormalizeProjectConfig(c *ProjectConfig) {
	if c == nil {
		return
	}

	if c.Options == nil {
		c.Options = make(map[string]map[string]any)
	}
	// `jobs` is deliberately NOT defaulted. It is an ignored surface, so it has no
	// canonical empty form worth materializing — and a non-nil empty map would
	// read as "this directory declares jobs" to the project-marker scan that keys
	// off `Jobs != nil`.
}

// ParseAndValidateProjectConfig combines strict parsing and validation.
func ParseAndValidateProjectConfig(data []byte) (*ProjectConfig, []diag.Diagnostic) {
	c, diags := ParseProjectConfig(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	vDiags := ValidateProjectConfig(c)
	diags = append(diags, vDiags...)

	NormalizeProjectConfig(c)

	return c, diags
}

// ParseScopeConfig decodes JSON data into a ScopeConfig using strict mode.
// Unknown fields are rejected and structured diagnostics are returned. The
// scope schema declares additionalProperties: false, so an out-of-contract key
// is a hard error.
func ParseScopeConfig(data []byte) (*ScopeConfig, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var c ScopeConfig
	if err := dec.Decode(&c); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse scope config: %v", err),
		}
	}
	return &c, nil
}

// ValidateScopeConfig checks structural invariants on a parsed scope config.
// The strict parser already rejects out-of-contract keys (the scope schema
// declares additionalProperties: false, and the Go ScopeConfig only models the
// inheritable fields). This validator adds the semantic checks the schema
// cannot express: included project paths must be relative to the scope
// directory, since an absolute or parent-escaping path breaks deterministic
// project membership.
func ValidateScopeConfig(c *ScopeConfig) []diag.Diagnostic {
	if c == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-config", "", "scope config is nil"),
		}
	}

	var diags []diag.Diagnostic
	for i, inc := range c.Includes {
		field := fmt.Sprintf("includes[%d]", i)
		if inc == "" || strings.HasPrefix(inc, "/") || strings.HasPrefix(inc, "../") || inc == ".." {
			diags = append(diags, diag.Errorf("invalid-include", field,
				"include %q must be a non-empty path relative to the scope directory", inc))
		}
	}
	diags = append(diags, validateScopeLine(c)...)
	diags = append(diags, validateDistribution(c.Distribution)...)
	return diags
}

// validateScopeLine checks the `line` block of a scope.
//
// A line names the git tags that version its projects, so its pattern has to
// render one tag per version and nothing else: exactly one {version}, and no
// character a tag cannot carry. And a line cannot sit on an activated scope —
// the scope-self project would then belong to the line it declares, which makes
// "a project's line is its NEAREST ancestor line" ambiguous for its own
// directory.
func validateScopeLine(c *ScopeConfig) []diag.Diagnostic {
	if c.Line == nil {
		return nil
	}
	if c.Activate {
		return []diag.Diagnostic{diag.Errorf("invalid-line", "line",
			"a scope that declares `line` must not also set `activate: true`; the scope-self project would be both the line and a member of it")}
	}
	tag := c.Line.Tag
	if tag == "" {
		return nil
	}

	var diags []diag.Diagnostic
	if strings.Count(tag, LineTagPlaceholder) != 1 {
		diags = append(diags, diag.Errorf("invalid-line", "line.tag",
			"line tag pattern %q must carry exactly one %s placeholder", tag, LineTagPlaceholder))
	}
	for _, r := range tag {
		if !lineTagAllowed(r) {
			diags = append(diags, diag.Errorf("invalid-line", "line.tag",
				"line tag pattern %q contains %q, which is outside [%s]", tag, string(r), LineTagCharset))
			break
		}
	}
	return diags
}

// ParseAndValidateScopeConfig combines strict parsing and validation.
func ParseAndValidateScopeConfig(data []byte) (*ScopeConfig, []diag.Diagnostic) {
	c, diags := ParseScopeConfig(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return c, append(diags, ValidateScopeConfig(c)...)
}

// sortedHookKeys returns the keys of a HookPhaseConfig map in sorted order.
func sortedHookKeys(m map[string]*HookPhaseConfig) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedRawKeys returns the keys of a raw-JSON map (`jobs`, `registries`) in
// sorted order, so a document produces the same diagnostics on every run.
func sortedRawKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedTaskKeys(m map[string]ProjectTaskTuning) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
