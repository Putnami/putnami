package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ProtocolVersion is the current workspace-config protocol version. The
// workspace/project configs carry no on-the-wire protocol version field — the
// strict parser accepts a single shape and rejects unknown fields — so this
// constant is the single anchor a bump has to move. The workspace declares no
// version of its own: a project's version is derived from the git tags of its
// version line (see LineConfig), so there is no authored semver to carry here.
// Pinned by conformance_test.go so any bump is intentional; bumping requires a
// migration story.
const ProtocolVersion = 1

// Config is the workspace-level putnami.workspace.json schema.
type Config struct {
	Schema         string                    `json:"$schema,omitempty"`
	Name           string                    `json:"name,omitempty"`
	Output         string                    `json:"output,omitempty"`
	Baseline       string                    `json:"baseline,omitempty"`
	EpicBranches   []string                  `json:"epicBranches,omitempty"`
	Profile        string                    `json:"profile,omitempty"`
	Quiet          *bool                     `json:"quiet,omitempty"`
	Verbose        *bool                     `json:"verbose,omitempty"`
	Options        map[string]map[string]any `json:"options,omitempty"`
	Includes       []string                  `json:"includes,omitempty"`
	Aliases        map[string]string         `json:"aliases,omitempty"`
	ProjectAliases map[string]string         `json:"projectAliases,omitempty"`
	Groups         map[string]string         `json:"groups,omitempty"`
	Extensions     ExtensionsConfig          `json:"extensions,omitempty"`
	// Registries declares one endpoint entry per ecosystem id. The workspace
	// schema validates only the outer map: the shape of each entry is owned by
	// the ecosystem profile the extension declares, so the value stays raw here.
	Registries     map[string]json.RawMessage `json:"registries,omitempty"`
	Templates      []string                   `json:"templates,omitempty"`
	AgentArtifacts []string                   `json:"agentArtifacts,omitempty"`
	Hooks          *HooksConfig               `json:"hooks,omitempty"`
	Disable        *DisableConfig             `json:"disable,omitempty"`
	Store          *StoreConfig               `json:"store,omitempty"`
	Sessions       *SessionsConfig            `json:"sessions,omitempty"`
}

// SessionsConfig tunes the per-worktree session store under
// <workspace>/.putnami/sessions. Unlike StoreConfig there is NO environment
// override: the build store is machine-global, while session records are
// per-worktree and a committed workspace config already reaches every worktree
// the records are written in.
type SessionsConfig struct {
	// Keep is how many session directories the store retains when it prunes.
	// A pointer so "unset" stays distinguishable from an authored value and the
	// default (20 in the CLI) applies. The schema's minimum of 1 rules out 0
	// deliberately: a retained 0 could be read as either "unlimited" or "delete
	// everything", and neither reading may be guessed.
	Keep *int `json:"keep,omitempty"`
}

// StoreConfig tunes the machine-global build store's garbage collection. These
// settings resolve with precedence env > workspace config > global config >
// default. The store LOCATION is set only via the PUTNAMI_STORE_DIR environment
// variable (it is machine-specific, so it does not belong in a committed config
// file). Pointer/empty fields mean "unset" so an explicit zero (e.g. gcGrace
// "0s", maxIdleBuilds 0) is distinguishable and honored.
type StoreConfig struct {
	// MaxBytes is the global byte budget across all per-repo stores.
	MaxBytes *int64 `json:"maxBytes,omitempty"`
	// GCGrace is a Go duration string (e.g. "1h", "30m") protecting recently-used
	// and in-flight entries from eviction.
	GCGrace string `json:"gcGrace,omitempty"`
	// MaxIdleBuilds evicts entries not hit in this many builds, regardless of
	// budget; 0 disables idle reclaim.
	MaxIdleBuilds *int64 `json:"maxIdleBuilds,omitempty"`
}

// ExtensionsConfig can be either []string or map[string]string, matching the
// workspace schema's oneOf. When serialized as an array, version constraints
// are empty strings. Names returns the extension names regardless of format.
type ExtensionsConfig struct {
	List map[string]string // name → version constraint ("" if no constraint)
}

// Names returns extension names as a string slice (deterministic, sorted order).
func (e ExtensionsConfig) Names() []string {
	if len(e.List) == 0 {
		return nil
	}
	names := make([]string, 0, len(e.List))
	for name := range e.List {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// UnmarshalJSON handles the extensions field which can be a JSON array of
// strings or a JSON object of name→constraint.
func (e *ExtensionsConfig) UnmarshalJSON(data []byte) error {
	e.List = make(map[string]string)

	// Try array first
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		for _, name := range arr {
			e.List[name] = ""
		}
		return nil
	}

	// Try object
	var obj map[string]string
	if err := json.Unmarshal(data, &obj); err == nil {
		e.List = obj
		return nil
	}

	return fmt.Errorf("extensions must be an array or object")
}

// MarshalJSON serializes ExtensionsConfig. If all constraints are empty,
// it serializes as a sorted array; otherwise as an object.
func (e ExtensionsConfig) MarshalJSON() ([]byte, error) {
	if e.List == nil {
		return []byte("null"), nil
	}
	// Array form is canonical when no entry carries a version constraint.
	allEmpty := true
	for _, v := range e.List {
		if v != "" {
			allEmpty = false
			break
		}
	}
	if allEmpty {
		return json.Marshal(e.Names())
	}
	return json.Marshal(e.List)
}

// HooksConfig defines lifecycle hooks at the workspace level.
type HooksConfig struct {
	CLI      *HookPhaseConfig            `json:"cli,omitempty"`
	Commands map[string]*HookPhaseConfig `json:"commands,omitempty"`
}

// HookPhaseConfig defines before/after hook phases.
//
// Every command listed here runs under the same execution contract an
// extension hook already had: bounded by a timeout, from the workspace
// root, through a NON-login shell. TimeoutMs and LoginShell are the two escape
// hatches from that contract, scoped to this hook group, and both are
// deliberately opt-IN so a workspace file that only lists commands behaves
// identically on every machine.
type HookPhaseConfig struct {
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
	// TimeoutMs bounds EACH command of this group, mirroring an extension
	// hook's timeoutMs. Unset and any value <= 0 mean the CLI default (120s):
	// a hook is a step of a build, not a daemon, and an unbounded hook wedges
	// every invocation in the workspace with Ctrl-C as the only exit.
	//
	// A pointer because config scopes merge: a global ~/.putnami/config.json
	// bound must survive a workspace file that only adds commands, and a
	// workspace file must still be able to state a bound of its own.
	TimeoutMs *int `json:"timeoutMs,omitempty"`
	// LoginShell runs this group's commands through `sh -lc` instead of
	// `sh -c`, sourcing /etc/profile and ~/.profile first.
	//
	// It exists only for hooks that genuinely need a version-manager PATH
	// (nvm, asdf, mise) that the user's profile installs. It is off by default
	// because a login shell makes the same repo and the same
	// putnami.workspace.json behave differently per developer dotfile, and
	// makes local diverge from CI — the reproducibility cost has to be a
	// choice recorded in the workspace file, not an accident of the runner.
	LoginShell *bool `json:"loginShell,omitempty"`
}

// DisableConfig controls workspace-level disabling of extensions, jobs, and tags.
type DisableConfig struct {
	Extensions []string `json:"extensions,omitempty"`
	Jobs       []string `json:"jobs,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

// Load reads and merges workspace-level config from:
// 1. ~/.putnami/config.json (global)
// 2. <workspaceRoot>/putnami.workspace.json
//
// A project's own putnami.json is deliberately NOT merged here. Project-level
// options are applied per project during plan-time param resolution
// (mergeParamLayers, via proj.Config.Options). Merging the current directory's
// project file into the workspace-wide Config leaked a single project's options
// (e.g. options.package.docker) onto every project — so running `--all` from
// inside a Docker-packaged workload scheduled docker packaging for all projects.
func Load(workspaceRoot string) *Config {
	merged := &Config{}

	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		globalPath := filepath.Join(homeDir, GlobalConfigDir, GlobalConfigFilename)
		if c := readWorkspaceConfigFile(globalPath); c != nil {
			MergeWorkspaceConfig(merged, c)
		}
	}

	if workspaceRoot != "" {
		path := ResolveFile(workspaceRoot, WorkspaceConfigFilename)
		if c := readWorkspaceConfigFile(path); c != nil {
			MergeWorkspaceConfig(merged, c)
		}
	}

	return merged
}

// GetCommandDefaults returns merged option defaults for a command.
// Merge order: * → commandName → extensionName → extensionName:commandName
func (c *Config) GetCommandDefaults(commandName, extensionName string) map[string]any {
	result := make(map[string]any)
	if c.Options == nil {
		return result
	}

	// 1. Global defaults
	if global, ok := c.Options["*"]; ok {
		for k, v := range global {
			result[k] = v
		}
	}

	// 2. Command defaults
	if cmd, ok := c.Options[commandName]; ok {
		for k, v := range cmd {
			result[k] = v
		}
	}

	if extensionName != "" {
		// 3. Extension defaults
		if ext, ok := c.Options[extensionName]; ok {
			for k, v := range ext {
				result[k] = v
			}
		}
		// 4. Extension:command defaults
		if extCmd, ok := c.Options[extensionName+":"+commandName]; ok {
			for k, v := range extCmd {
				result[k] = v
			}
		}
	}

	return result
}

// RegistryEntry returns the raw registry entry declared for an ecosystem.
// The second result is false when the workspace declares none, which a caller
// must distinguish from an entry that is present and empty.
func (c *Config) RegistryEntry(ecosystem string) (json.RawMessage, bool) {
	if c == nil || c.Registries == nil {
		return nil, false
	}
	entry, ok := c.Registries[ecosystem]
	return entry, ok
}

// MergeWorkspaceConfig merges source into target. Source values take precedence.
func MergeWorkspaceConfig(target, source *Config) {
	if source.Name != "" {
		target.Name = source.Name
	}
	if source.Output != "" {
		target.Output = source.Output
	}
	if source.Baseline != "" {
		target.Baseline = source.Baseline
	}
	if len(source.EpicBranches) > 0 {
		target.EpicBranches = mergeUniqueStrings(target.EpicBranches, source.EpicBranches)
	}
	if source.Profile != "" {
		target.Profile = source.Profile
	}
	if source.Quiet != nil {
		target.Quiet = source.Quiet
	}
	if source.Verbose != nil {
		target.Verbose = source.Verbose
	}
	if len(source.Includes) > 0 {
		target.Includes = mergeUniqueStrings(target.Includes, source.Includes)
	}
	if len(source.Extensions.List) > 0 {
		if target.Extensions.List == nil {
			target.Extensions.List = make(map[string]string)
		}
		for name, constraint := range source.Extensions.List {
			target.Extensions.List[name] = constraint
		}
	}
	if len(source.Templates) > 0 {
		target.Templates = mergeUniqueStrings(target.Templates, source.Templates)
	}
	if len(source.AgentArtifacts) > 0 {
		target.AgentArtifacts = mergeUniqueStrings(target.AgentArtifacts, source.AgentArtifacts)
	}
	if source.Options != nil {
		if target.Options == nil {
			target.Options = make(map[string]map[string]any)
		}
		for cmd, opts := range source.Options {
			if target.Options[cmd] == nil {
				target.Options[cmd] = make(map[string]any)
			}
			for k, v := range opts {
				target.Options[cmd][k] = v
			}
		}
	}
	// A registry entry is replaced WHOLE, per ecosystem: its shape belongs to
	// the ecosystem profile, so a deep merge here would invent a document no
	// profile validates.
	if len(source.Registries) > 0 {
		if target.Registries == nil {
			target.Registries = make(map[string]json.RawMessage, len(source.Registries))
		}
		for ecosystem, entry := range source.Registries {
			target.Registries[ecosystem] = entry
		}
	}
	target.Aliases = mergeStringMap(target.Aliases, source.Aliases)
	target.ProjectAliases = mergeStringMap(target.ProjectAliases, source.ProjectAliases)
	target.Groups = mergeStringMap(target.Groups, source.Groups)
	if source.Hooks != nil {
		target.Hooks = mergeHooksConfig(target.Hooks, source.Hooks)
	}
	if source.Disable != nil {
		target.Disable = mergeDisableConfig(target.Disable, source.Disable)
	}
	if source.Store != nil {
		target.Store = mergeStoreConfig(target.Store, source.Store)
	}
	if source.Sessions != nil {
		target.Sessions = mergeSessionsConfig(target.Sessions, source.Sessions)
	}
}

// mergeSessionsConfig overlays source's set session fields onto target (a later
// scope wins per field), leaving unset fields intact.
func mergeSessionsConfig(target, source *SessionsConfig) *SessionsConfig {
	if target == nil {
		target = &SessionsConfig{}
	}
	if source.Keep != nil {
		target.Keep = source.Keep
	}
	return target
}

// mergeStoreConfig overlays source's set store fields onto target (a later scope
// wins per field), leaving unset fields intact.
func mergeStoreConfig(target, source *StoreConfig) *StoreConfig {
	if target == nil {
		target = &StoreConfig{}
	}
	if source.MaxBytes != nil {
		target.MaxBytes = source.MaxBytes
	}
	if source.GCGrace != "" {
		target.GCGrace = source.GCGrace
	}
	if source.MaxIdleBuilds != nil {
		target.MaxIdleBuilds = source.MaxIdleBuilds
	}
	return target
}

func readWorkspaceConfigFile(path string) *Config {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return &c
}

func mergeStringMap(target, source map[string]string) map[string]string {
	if source == nil {
		return target
	}
	if target == nil {
		target = make(map[string]string, len(source))
	}
	for k, v := range source {
		target[k] = v
	}
	return target
}

func mergeHooksConfig(target, source *HooksConfig) *HooksConfig {
	if target == nil {
		target = &HooksConfig{}
	}
	if source.CLI != nil {
		target.CLI = mergeHookPhase(target.CLI, source.CLI)
	}
	if source.Commands != nil {
		if target.Commands == nil {
			target.Commands = make(map[string]*HookPhaseConfig)
		}
		for cmd, phase := range source.Commands {
			target.Commands[cmd] = mergeHookPhase(target.Commands[cmd], phase)
		}
	}
	return target
}

func mergeHookPhase(target, source *HookPhaseConfig) *HookPhaseConfig {
	if target == nil {
		target = &HookPhaseConfig{}
	}
	if len(source.Before) > 0 {
		target.Before = append(target.Before, source.Before...)
	}
	if len(source.After) > 0 {
		target.After = append(target.After, source.After...)
	}
	// Command lists accumulate across scopes; the execution contract does not.
	// A later scope that states a bound or a shell mode replaces the earlier
	// one, and one that stays silent leaves it alone — which is why both are
	// pointers (same rule as Quiet/Verbose and StoreConfig above).
	if source.TimeoutMs != nil {
		target.TimeoutMs = source.TimeoutMs
	}
	if source.LoginShell != nil {
		target.LoginShell = source.LoginShell
	}
	return target
}

func mergeDisableConfig(target, source *DisableConfig) *DisableConfig {
	if target == nil {
		target = &DisableConfig{}
	}
	if len(source.Extensions) > 0 {
		target.Extensions = mergeUniqueStrings(target.Extensions, source.Extensions)
	}
	if len(source.Jobs) > 0 {
		target.Jobs = mergeUniqueStrings(target.Jobs, source.Jobs)
	}
	if len(source.Tags) > 0 {
		target.Tags = mergeUniqueStrings(target.Tags, source.Tags)
	}
	return target
}

func mergeUniqueStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	result := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			result = append(result, s)
		}
	}
	for _, s := range b {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			result = append(result, s)
		}
	}
	return result
}
