package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ScopeConfig is an intermediate putnami.json that defines inheritable
// config for all projects below it. It has a strict schema boundary — it never
// reads workspace-only fields (output, quiet, verbose, options, disable, hooks).
//
// A scope with non-empty Includes is an "autonomous scope" — it declares
// its own sub-projects (relative paths within the scope's directory). A scope
// without Includes is an "inherited scope" (tags/extensions only).
//
// When Activate is true, the scope directory is also registered as an
// activation target — the same putnami.json that lists Includes also becomes
// a project, allowing extensions to run against scope-shaped artifacts (e.g.
// workspace-level infra) without inventing a fake leaf.
type ScopeConfig struct {
	Schema         string                    `json:"$schema,omitempty"`
	Line           *LineConfig               `json:"line,omitempty"`
	Activate       bool                      `json:"activate,omitempty"`
	Includes       []string                  `json:"includes,omitempty"`
	Tags           []string                  `json:"tags,omitempty"`
	Extensions     []string                  `json:"extensions,omitempty"`
	PublishConfig  map[string]map[string]any `json:"publishConfig,omitempty"`
	ProjectAliases map[string]string         `json:"projectAliases,omitempty"`
	Groups         map[string]string         `json:"groups,omitempty"`
	// Distribution is the distribution level every project below this scope
	// inherits unless it declares its own. The deepest scope wins.
	Distribution *DistributionConfig `json:"distribution,omitempty"`
}

// LineConfig declares that a scope is a version line: its projects are
// versioned and tagged together, and the version of a commit is derived from
// the line's git tags rather than from any declared field.
type LineConfig struct {
	// Tag is the pattern of the line's git tags. It carries exactly one
	// {version} placeholder and is otherwise made of [A-Za-z0-9._/{}-].
	// Empty means the default pattern, "<scope path>/v{version}".
	Tag string `json:"tag,omitempty"`
}

// LineTagPlaceholder is the only placeholder a line tag pattern may carry.
const LineTagPlaceholder = "{version}"

// LineTagCharset is the set of characters a line tag pattern may use, as a
// character-class body. The placeholder braces are part of it; nothing else
// outside this set is accepted.
const LineTagCharset = "A-Za-z0-9._/{}-"

// lineTagAllowed reports whether r may appear in a line tag pattern.
func lineTagAllowed(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '/', r == '{', r == '}', r == '-':
		return true
	}
	return false
}

// IsLine reports whether this scope declares a version line.
func (sc *ScopeConfig) IsLine() bool {
	return sc != nil && sc.Line != nil
}

// LineTagPattern returns the tag pattern of the line a scope declares.
// scopePath is the line scope's path relative to the workspace root, in slash
// form; the empty path is the workspace root, whose default pattern is
// "v{version}". A line that states its own tag keeps it verbatim.
func LineTagPattern(scopePath string, line *LineConfig) string {
	if line != nil && line.Tag != "" {
		return line.Tag
	}
	scopePath = strings.Trim(scopePath, "/")
	if scopePath == "" {
		return "v" + LineTagPlaceholder
	}
	return scopePath + "/v" + LineTagPlaceholder
}

// RenderLineTag substitutes a version into a line tag pattern.
func RenderLineTag(pattern, version string) string {
	return strings.Replace(pattern, LineTagPlaceholder, version, 1)
}

// ParseLineTag reads a git tag back into the line that produced it and the
// version it carries. lines maps each line's scope path to its tag pattern, as
// LineTagPattern returns it. The most specific pattern wins — the one whose
// literal prefix is longest — so "ts/v{version}" claims "ts/v0.3.0" even when
// the root line "v{version}" is also declared. Ties break on the scope path so
// one repository always resolves one tag the same way.
func ParseLineTag(tag string, lines map[string]string) (scopePath, version string, ok bool) {
	bestPrefix := -1
	for _, path := range sortedMapKeys(lines) {
		prefix, suffix, found := strings.Cut(lines[path], LineTagPlaceholder)
		if !found {
			continue
		}
		if len(tag) <= len(prefix)+len(suffix) {
			continue
		}
		if !strings.HasPrefix(tag, prefix) || !strings.HasSuffix(tag, suffix) {
			continue
		}
		if len(prefix) <= bestPrefix {
			continue
		}
		bestPrefix = len(prefix)
		scopePath = path
		version = tag[len(prefix) : len(tag)-len(suffix)]
		ok = true
	}
	return scopePath, version, ok
}

// IncludePaths returns the scope's project membership entries.
func (sc *ScopeConfig) IncludePaths() []string {
	if sc == nil {
		return nil
	}
	return sc.Includes
}

// ScopeIndex is the aggregated result of scanning all scopes in the workspace.
type ScopeIndex struct {
	// ProjectAliases maps alias → target ID, aggregated from all scopes.
	ProjectAliases map[string]string
	// Groups maps group name → pattern, aggregated from all scopes.
	Groups map[string]string
	// Lines maps each version line's scope path (slash-separated, relative to
	// the workspace root) to its tag pattern. A workspace where no scope
	// declares a line is one line: {"": "v{version}"}.
	Lines map[string]string
}

// LoadScopeChain walks from projectDir up to workspaceRoot, merging
// intermediate scope configs. Deepest (closest to project) wins.
// Returns nil if no scopes are found.
func LoadScopeChain(workspaceRoot, projectDir string) *ScopeConfig {
	merged, _ := LoadScopeChainWithSources(workspaceRoot, projectDir)
	return merged
}

// LoadScopeChainWithSources is LoadScopeChain plus the scope config files the
// merge read, as workspace-relative slash paths, shallowest first. A file that
// exists but declares no scope field is not a source: it contributed nothing,
// and readScopeFile already treats it as absent.
//
// The sources are what makes a scope config an input of the projects below
// it: every field the merge copies onto a project — tags, extensions, the
// name pattern — came from one of these files, so a change to any of them is
// a change to that project. Both answers come from ONE walk, so the
// sources cannot name a file the merge did not read.
func LoadScopeChainWithSources(workspaceRoot, projectDir string) (*ScopeConfig, []string) {
	// Collect ancestor dirs from workspace root down to project dir (exclusive of project itself).
	var chain []string
	dir := projectDir
	for {
		parent := filepath.Dir(dir)
		if parent == dir || !isSubpath(workspaceRoot, parent) {
			break
		}
		dir = parent
		chain = append(chain, dir)
	}

	// Reverse so we iterate root → ... → parent (shallowest first, deepest wins).
	sort.Slice(chain, func(i, j int) bool {
		return len(chain[i]) < len(chain[j])
	})

	var merged *ScopeConfig
	var sources []string
	for _, ancestor := range chain {
		file := ResolveFile(ancestor, ConfigFilename)
		sc := readScopeFile(file)
		if sc == nil {
			continue
		}
		if merged == nil {
			merged = &ScopeConfig{}
		}
		MergeScope(merged, sc)
		if rel, err := filepath.Rel(workspaceRoot, file); err == nil {
			sources = append(sources, filepath.ToSlash(rel))
		}
	}

	return merged, sources
}

// ScanAllScopes scans the workspace tree for all scope config files
// that contain scope fields (projectAliases, groups, line). Aggregates them
// into a ScopeIndex and validates uniqueness. Returns an error if duplicate
// aliases or groups are found, or if a version line is declared under another
// version line — a project's line has to be exactly one, so a nested line
// would leave every project below it with two.
func ScanAllScopes(workspaceRoot string, projectPaths []string) (*ScopeIndex, error) {
	index := &ScopeIndex{
		ProjectAliases: make(map[string]string),
		Groups:         make(map[string]string),
		Lines:          make(map[string]string),
	}

	// Track sources for conflict detection: name → source dir.
	aliasSources := make(map[string]string)
	groupSources := make(map[string]string)

	// Collect all unique directories to scan (ancestors of all projects + workspace root).
	dirs := collectScopeDirs(workspaceRoot, projectPaths)

	for _, dir := range dirs {
		sc := readScopeFile(ResolveFile(dir, ConfigFilename))
		if sc == nil {
			continue
		}

		relDir, _ := filepath.Rel(workspaceRoot, dir)
		if relDir == "." {
			relDir = "(workspace root)"
		} else if sc.IsLine() {
			scopePath := filepath.ToSlash(relDir)
			index.Lines[scopePath] = LineTagPattern(scopePath, sc.Line)
		}

		// Iterate in sorted order for deterministic error messages.
		aliasNames := sortedMapKeys(sc.ProjectAliases)
		for _, alias := range aliasNames {
			target := sc.ProjectAliases[alias]
			if existingSource, exists := aliasSources[alias]; exists {
				return nil, fmt.Errorf("project alias %q defined in both %s and %s", alias, existingSource, relDir)
			}
			aliasSources[alias] = relDir
			index.ProjectAliases[alias] = target
		}

		groupNames := sortedMapKeys(sc.Groups)
		for _, group := range groupNames {
			pattern := sc.Groups[group]
			if existingSource, exists := groupSources[group]; exists {
				return nil, fmt.Errorf("group %q defined in both %s and %s", group, existingSource, relDir)
			}
			groupSources[group] = relDir
			index.Groups[group] = pattern
		}
	}

	// Also merge workspace-level projectAliases and groups.
	wsCfg := readWorkspaceConfigFile(ResolveFile(workspaceRoot, WorkspaceConfigFilename))
	if wsCfg != nil {
		wsAliasNames := sortedMapKeys(wsCfg.ProjectAliases)
		for _, alias := range wsAliasNames {
			target := wsCfg.ProjectAliases[alias]
			if existingSource, exists := aliasSources[alias]; exists {
				return nil, fmt.Errorf("project alias %q defined in both %s and %s", alias, existingSource, "(workspace root)")
			}
			aliasSources[alias] = "(workspace root)"
			index.ProjectAliases[alias] = target
		}
		wsGroupNames := sortedMapKeys(wsCfg.Groups)
		for _, group := range wsGroupNames {
			pattern := wsCfg.Groups[group]
			if existingSource, exists := groupSources[group]; exists {
				return nil, fmt.Errorf("group %q defined in both %s and %s", group, existingSource, "(workspace root)")
			}
			groupSources[group] = "(workspace root)"
			index.Groups[group] = pattern
		}
	}

	if err := checkNestedLines(index.Lines); err != nil {
		return nil, err
	}
	if len(index.Lines) == 0 {
		index.Lines[""] = LineTagPattern("", nil)
	}

	return index, nil
}

// checkNestedLines refuses a version line declared under another one. Lines are
// compared as directory paths, so "go" contains "go/framework" but not "golang".
func checkNestedLines(lines map[string]string) error {
	paths := sortedMapKeys(lines)
	for i, outer := range paths {
		for _, inner := range paths[i+1:] {
			if strings.HasPrefix(inner, outer+"/") {
				return fmt.Errorf("version line %q is declared under version line %q; a project's line must be exactly one", inner, outer)
			}
		}
	}
	return nil
}

// ReadScopeConfig reads and returns the ScopeConfig from a directory's config file.
// Returns nil if the file doesn't exist or has no scope fields.
func ReadScopeConfig(dir string) *ScopeConfig {
	return readScopeFile(ResolveFile(dir, ConfigFilename))
}

// ResolveNamePattern returns the canonical project name derived from the
// scope chain's publishConfig namePattern. It looks for the first channel
// with a "namePattern" field and replaces "{name}" with the given slug
// (typically the directory basename). Returns "" if no namePattern is found.
func (sc *ScopeConfig) ResolveNamePattern(slug string) string {
	if sc == nil || sc.PublishConfig == nil {
		return ""
	}
	// Iterate channels in sorted order for determinism.
	channels := make([]string, 0, len(sc.PublishConfig))
	for ch := range sc.PublishConfig {
		channels = append(channels, ch)
	}
	sort.Strings(channels)

	for _, ch := range channels {
		cfg := sc.PublishConfig[ch]
		if pattern, ok := cfg["namePattern"].(string); ok && pattern != "" {
			return strings.Replace(pattern, "{name}", slug, 1)
		}
	}
	return ""
}

// MergeScope merges source into target. Deepest scope wins for
// publishConfig, extensions, distribution; tags are merged (union).
func MergeScope(target, source *ScopeConfig) {

	// Line is deliberately NOT merged. A line is a property of the scope that
	// declares it, not something a deeper scope or a project inherits: a project
	// belongs to its NEAREST ancestor line, and copying the block down the chain
	// would turn every intermediate scope into a line of its own.

	// Tags: merge (union)
	if len(source.Tags) > 0 {
		target.Tags = mergeUniqueStrings(target.Tags, source.Tags)
	}

	// Extensions: deepest wins (replace)
	if len(source.Extensions) > 0 {
		target.Extensions = source.Extensions
	}

	// PublishConfig: deepest wins per channel
	if source.PublishConfig != nil {
		if target.PublishConfig == nil {
			target.PublishConfig = make(map[string]map[string]any)
		}
		for channel, cfg := range source.PublishConfig {
			target.PublishConfig[channel] = cfg
		}
	}

	// Distribution: deepest wins. A block that states no level still wins, so
	// the reader refuses it instead of silently falling back to a shallower one.
	if source.Distribution != nil {
		target.Distribution = &DistributionConfig{Visibility: source.Distribution.Visibility}
	}

	// ProjectAliases: aggregated (not inherited per project)
	// Groups: aggregated (not inherited per project)
	// These are handled by ScanAllScopes, not per-project chain.
}

// collectScopeDirs returns all unique directories between workspace root
// and project dirs that might contain scope configs.
func collectScopeDirs(workspaceRoot string, projectPaths []string) []string {
	seen := make(map[string]bool)
	var dirs []string

	for _, relPath := range projectPaths {
		dir := filepath.Dir(filepath.Join(workspaceRoot, relPath))
		for dir != workspaceRoot && isSubpath(workspaceRoot, dir) {
			if seen[dir] {
				break
			}
			seen[dir] = true
			dirs = append(dirs, dir)
			dir = filepath.Dir(dir)
		}
	}

	sort.Strings(dirs)
	return dirs
}

func readScopeFile(path string) *ScopeConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var sc ScopeConfig
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil
	}
	// Only return if it has scope-specific fields.
	if !sc.Activate && sc.Line == nil && len(sc.Includes) == 0 && len(sc.Tags) == 0 && len(sc.Extensions) == 0 &&
		sc.PublishConfig == nil && sc.ProjectAliases == nil && sc.Groups == nil && sc.Distribution == nil {
		return nil
	}
	return &sc
}

// sortedMapKeys returns the keys of a map in sorted order for deterministic iteration.
func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isSubpath returns true if child is under parent (or equal to it).
func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && len(rel) > 0 && rel[0] != '.'
}
