package extension

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	model "go.putnami.dev/cli/model/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// DiscoveryResult is the full outcome of extension discovery: the loaded
// extensions plus the records of genuinely-unloadable ones.
type DiscoveryResult struct {
	Extensions []*ExtensionDescription
	Skipped    []SkippedExtension
	// RemovedCapabilities are the provider capabilities discovery removed
	// from extensions that loaded and stay in Extensions
	// (withoutProviderCapabilities): a hosted run's path extensions, and every
	// dependency the workspace does not declare. They are not load failures,
	// so they are kept apart from Skipped; ProviderCause names them where a
	// provider is absent.
	RemovedCapabilities []RemovedCapability
	// Dependencies are the extensions of the workspace's graph that
	// putnami.workspace.json does not declare (ADR 0066), loaded or not. A
	// dependency that did not load is also in Skipped, naming its dependent.
	Dependencies []GraphDependency
}

// MissingDependencies returns the names of the graph's dependencies that did
// not load, sorted.
func (d *DiscoveryResult) MissingDependencies() []string {
	if d == nil {
		return nil
	}
	var names []string
	for _, dep := range d.Dependencies {
		if !dep.Loaded {
			names = append(names, dep.Name)
		}
	}
	sort.Strings(names)
	return names
}

// DiscoverExtensions scans extension paths and builds an extension registry.
// It is the summary API over DiscoverExtensionsDetailed for call sites that do
// not consume skip records.
func DiscoverExtensions(workspaceRoot string, cfg *wsproto.Config, projectPaths []string) ([]*ExtensionDescription, error) {
	result, err := DiscoverExtensionsDetailed(workspaceRoot, cfg, projectPaths)
	if err != nil {
		return nil, err
	}
	return result.Extensions, nil
}

// DiscoverExtensionsDetailed scans extension paths and builds an extension
// registry. Sources:
//  1. Workspace projects that have a putnami.extension.json, except one whose
//     name the workspace config pins by name
//  2. Explicitly referenced extensions from workspace config
//     (checked as an absolute path, a workspace path, and .putnami/extensions/ via lock file)
//  3. A project manifest source 1 set aside whose pinned build did not load
//  4. The dependencies of the declared extensions, from the install layout
//     (dependencyDiscovery)
//
// No package manager directory and no package.json is a source: an extension
// loads only because it is a workspace extension project (source 1, which
// needs no name in the workspace config), or because the workspace config, the
// lock or a dependency names it.
//
// A name is registered once, by the first source that loads it. A workspace
// config key equal to a project manifest's name, and that is not that
// project's path, pins the published build: it wins over the project manifest,
// which then loads only when the pinned build does not.
//
// Extensions whose manifest exists but cannot be loaded (parse error or
// contract-too-new) are returned in Skipped rather than silently dropped; an
// explicitly referenced extension is only recorded as skipped when no probe
// location loaded it.
//
// A hosted run (runcredential.Hosted) reads a workspace config key as a
// workspace path only when the key is path-shaped (declaredByPath): any other
// key names an extension, which loads from the artifact store, and a workspace
// project of that name never loads in place of a pinned build that is not
// installed (settlePinnedProjects). It keeps the extensions installed from the
// artifact store (InArtifactStore) and the workspace's own path extensions
// (WorkspacePathExtension), and returns every other one in Skipped. A path
// extension loses its provider capabilities, each recorded in
// RemovedCapabilities: no repository code can receive a credential, and the
// engine starts a path extension only after custody ended.
func DiscoverExtensionsDetailed(workspaceRoot string, cfg *wsproto.Config, projectPaths []string) (*DiscoveryResult, error) {
	var extensions []*ExtensionDescription
	var skipped []SkippedExtension
	hosted := runcredential.Hosted()
	seen := make(map[string]bool)
	seenSkip := make(map[string]bool)
	recordSkips := func(skips []SkippedExtension) {
		for _, s := range skips {
			key := s.Path + "\x00" + s.Reason.Error()
			if seenSkip[key] {
				continue
			}
			seenSkip[key] = true
			skipped = append(skipped, s)
		}
	}

	// Read lock file once for resolving installed extension paths.
	//
	// Tolerated, but not silent: without a lock the installed (.putnami/…) probe
	// resolves nothing, so every lock-pinned registry extension quietly drops
	// out of discovery and the run's only symptom is a shorter plan. Since the
	// format floor moved to v2, a v1 lock produces exactly
	// that, so the reason is logged — once per process, like the other discovery
	// warnings, because discovery runs from ~5 call sites per invocation
	// (B6r/F6).
	lockFile, lockErr := lockfile.ReadLockFile(workspaceRoot)
	if lockErr != nil {
		warnOnce("lockfile:"+workspaceRoot+":"+lockErr.Error(),
			"lock file could not be read; lock-pinned extensions were not discovered",
			"workspace", workspaceRoot, "error", lockErr)
	}

	// 1. Scan workspace projects for extension manifests
	projectExts, pinnedLocal, projectSkips := scanProjectExtensions(workspaceRoot, cfg, projectPaths)
	recordSkips(projectSkips)
	graph := &dependencyDiscovery{
		workspaceRoot: workspaceRoot, cfg: cfg, lockFile: lockFile, hosted: hosted,
		installed: make(map[*ExtensionDescription]bool), projects: make(map[*ExtensionDescription]bool),
	}
	for _, ext := range projectExts {
		seen[ext.Name] = true
		graph.projects[ext] = true
		extensions = append(extensions, ext)
	}

	// 2. Explicitly referenced extensions
	for _, extRef := range cfg.Extensions.Names() {
		if seen[extRef] {
			continue
		}
		var ext *ExtensionDescription
		// Skips from earlier probe locations only count when no later probe
		// loads the ref — an extension that loaded is not skipped.
		var refSkips []SkippedExtension
		collect := func(skip *SkippedExtension) {
			if skip != nil {
				refSkips = append(refSkips, *skip)
			}
		}
		// Try as an absolute local extension path first. Workspace configs
		// support both absolute extension paths and the repo-local shorthand
		// "/go/extension", so preserve the absolute-path behavior before
		// applying the workspace-relative fallback below.
		if filepath.IsAbs(extRef) {
			var skip *SkippedExtension
			ext, skip = tryLoadExtension(extRef, extRef, false)
			collect(skip)
			if ext != nil && !seen[ext.Name] {
				ext.LocalSource = true
				seen[ext.Name] = true
				extensions = append(extensions, ext)
				continue
			}
		}
		// A ref that names a volume is a Windows absolute (or drive-relative)
		// path, and only the absolute probe above can load it: joined under the
		// workspace or the install layout it names nothing.
		if volume := filepath.VolumeName(extRef); volume != "" {
			if ext == nil && len(refSkips) == 0 {
				refSkips = append(refSkips, SkippedExtension{
					Ref: extRef, Name: extRef, Path: extRef,
					Reason: fmt.Errorf("extension reference %q names volume %s; it loads only as an absolute path that holds %s",
						extRef, volume, ManifestFilename),
				})
			}
			recordSkips(refSkips)
			continue
		}
		// Try as a workspace project path. A hosted run reads only a
		// path-shaped key as a path, so a key that names an extension loads
		// its build from the artifact store, never a workspace directory of
		// that name.
		if relPath, ok := normalizeWorkspaceExtensionPath(extRef); ok && (!hosted || declaredByPath(extRef)) {
			absPath := filepath.Join(workspaceRoot, relPath)
			var skip *SkippedExtension
			ext, skip = tryLoadExtension(absPath, extRef, false)
			collect(skip)
			if ext != nil && !seen[ext.Name] {
				ext.RelPath = relPath
				ext.LocalSource = true
				seen[ext.Name] = true
				extensions = append(extensions, ext)
				continue
			}
		}
		// Try as an installed extension via stable symlink or artifact directory
		var skip *SkippedExtension
		ext, skip = tryLoadInstalledExtension(workspaceRoot, extRef, lockFile)
		collect(skip)
		if ext != nil && !seen[ext.Name] {
			seen[ext.Name] = true
			graph.installed[ext] = true
			extensions = append(extensions, ext)
			continue
		}
		recordSkips(refSkips)
	}

	// 3. A project manifest set aside for a pin that loaded nothing.
	extensions, unsettled := settlePinnedProjects(workspaceRoot, extensions, pinnedLocal, seen, hosted)
	recordSkips(unsettled)

	// 4. The dependencies of the declared extensions.
	extensions, dependencies, unloaded, dependencyRemoved := graph.run(extensions)
	recordSkips(unloaded)

	extensions, notHosted, removed := keepHostedExtensions(workspaceRoot, extensions)
	recordSkips(notHosted)
	removed = append(dependencyRemoved, removed...)

	// Propagate RelPath to all job definitions as ExtensionPath
	for _, ext := range extensions {
		if ext.RelPath == "" {
			continue
		}
		for _, job := range ext.Jobs {
			if job.ExtensionPath == "" {
				job.ExtensionPath = ext.RelPath
			}
		}
	}

	// A loaded vocabulary gives its verbs' default traits to the jobs whose
	// manifest declared none, across extensions.
	applyVocabularyTraits(extensions)

	return &DiscoveryResult{Extensions: extensions, Skipped: skipped, RemovedCapabilities: removed, Dependencies: dependencies}, nil
}

// scanProjectExtensions loads the manifest at each workspace project root. A
// manifest whose name the workspace config pins by name is returned apart, in
// pinned, for settlePinnedProjects; every other name is loaded once, by the
// first project that declares it.
func scanProjectExtensions(workspaceRoot string, cfg *wsproto.Config, projectPaths []string) (loaded, pinned []*ExtensionDescription, skipped []SkippedExtension) {
	names := make(map[string]bool)
	for _, projPath := range projectPaths {
		relPath, ok := normalizeWorkspaceExtensionPath(projPath)
		if !ok {
			continue
		}
		ext, skip := tryLoadExtension(filepath.Join(workspaceRoot, relPath), relPath, false)
		if skip != nil {
			skipped = append(skipped, *skip)
		}
		if ext == nil || names[ext.Name] {
			continue
		}
		names[ext.Name] = true
		ext.RelPath = relPath
		ext.LocalSource = true
		if pinnedByName(cfg, ext.Name, relPath) {
			pinned = append(pinned, ext)
			continue
		}
		loaded = append(loaded, ext)
	}
	return loaded, pinned, skipped
}

// settlePinnedProjects resolves each project manifest scanProjectExtensions
// set aside. When the pinned build loaded, that build and its jobs record the
// project it replaces, so a project that names that project's path keeps
// running the extension. Otherwise the project manifest loads, with a warning
// that the pin is not installed; on a hosted run it does not, and the project
// is returned in skipped, with errPinnedBuildMissing.
func settlePinnedProjects(workspaceRoot string, extensions, pinned []*ExtensionDescription, seen map[string]bool, hosted bool) ([]*ExtensionDescription, []SkippedExtension) {
	var skipped []SkippedExtension
	for _, local := range pinned {
		if seen[local.Name] {
			if ext := FindExtensionByName(extensions, local.Name); ext != nil && !ext.LocalSource {
				ext.PinnedOver = local.RelPath
				for _, job := range ext.Jobs {
					if job.ExtensionPath == "" {
						job.ExtensionPath = local.RelPath
					}
				}
			}
			continue
		}
		if hosted {
			skipped = append(skipped, SkippedExtension{
				Ref: local.Name, Name: local.Name, Path: local.Path, Version: local.Version,
				Reason: fmt.Errorf("the workspace config pins %s, and its build is not installed: %w", local.Name, errPinnedBuildMissing),
			})
			continue
		}
		warnOnce("pin-unloaded:"+workspaceRoot+":"+local.Name,
			"workspace config pins an extension whose build is not installed; using the workspace project's manifest instead — run putnami install",
			"extension", local.Name, "project", filepath.ToSlash(local.RelPath))
		seen[local.Name] = true
		extensions = append(extensions, local)
	}
	return extensions, skipped
}

// pinnedByName reports whether the workspace config names the extension by
// its manifest name with a key that is not the project's own path. Such a key
// is a registry pin, so the project's manifest yields to the pinned build. A
// hosted run reads only a path-shaped key as a path (declaredByPath).
func pinnedByName(cfg *wsproto.Config, name, projectRelPath string) bool {
	if cfg == nil {
		return false
	}
	if _, ok := cfg.Extensions.List[name]; !ok {
		return false
	}
	if runcredential.Hosted() && !declaredByPath(name) {
		return true
	}
	if rel, ok := normalizeWorkspaceExtensionPath(name); ok && rel == filepath.Clean(projectRelPath) {
		return false
	}
	return true
}

// normalizeWorkspaceExtensionPath returns ref as a clean workspace-relative
// path. A leading separator is the repo-local shorthand ("/go/extension"). A ref
// that escapes the workspace, or that names a volume (a Windows drive or UNC
// share), has no workspace-relative form.
func normalizeWorkspaceExtensionPath(ref string) (string, bool) {
	if filepath.VolumeName(ref) != "" {
		return "", false
	}
	rel := strings.TrimLeft(ref, `/\`)
	if rel == "" {
		return "", false
	}
	rel = filepath.FromSlash(strings.ReplaceAll(rel, `\`, `/`))
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// tryLoadInstalledExtension attempts to load an extension from the symlink-based
// layout: .putnami/bin/extensions/{name} (stable symlink) or
// .putnami/bin/artifacts/extensions/{name}@{version}/ (direct artifact).
func tryLoadInstalledExtension(workspaceRoot, name string, lockFile *lockfile.LockFile) (*ExtensionDescription, *SkippedExtension) {
	// Try stable symlink path
	stableDir := layout.StableDir(workspaceRoot, layout.Extensions, name)
	ext, skip := tryLoadExtension(stableDir, name, true)
	if ext != nil || skip != nil {
		if skip != nil && skip.Version == "" && lockFile != nil {
			if entry, ok := lockFile.GetExtension(name); ok {
				skip.Version = entry.Version
			}
		}
		return ext, skip
	}

	// Try exact version in artifact layout
	if lockFile != nil {
		if entry, ok := lockFile.GetExtension(name); ok {
			artifactDir := layout.ArtifactDir(workspaceRoot, layout.Extensions, name, entry.Version)
			ext, skip = tryLoadExtension(artifactDir, name, true)
			if skip != nil && skip.Version == "" {
				skip.Version = entry.Version
			}
			return ext, skip
		}
	}

	return nil, nil
}

// LoadExtensionFromDir loads an extension manifest from a directory and resolves
// the same canonical metadata used by discovery.
func LoadExtensionFromDir(absPath, refName string) *ExtensionDescription {
	ext, _ := tryLoadExtension(absPath, refName, false)
	if ext != nil {
		ext.LocalSource = true
	}
	return ext
}

// SelectPreparedExtensions replaces ambient discovery for registry refs whose
// read surface was already selected from the workspace lock. A non-empty root
// is loaded as an installed extension; an empty root means exact preparation
// failed and the ref is removed. Unrelated local extensions remain untouched.
// This keeps read consumers from choosing a stale stable link
// after the bounded preparation pass made a lock-faithful decision.
func SelectPreparedExtensions(discovered []*ExtensionDescription, prepared map[string]string) []*ExtensionDescription {
	if len(prepared) == 0 {
		return discovered
	}
	selected := make([]*ExtensionDescription, 0, len(discovered)+len(prepared))
	for _, ext := range discovered {
		if ext == nil {
			continue
		}
		if _, controlled := prepared[ext.Name]; controlled {
			continue
		}
		selected = append(selected, ext)
	}
	names := make([]string, 0, len(prepared))
	for name := range prepared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		root := prepared[name]
		if root == "" {
			continue
		}
		ext, _ := tryLoadExtension(root, name, true)
		if ext == nil || ext.Name != name {
			continue
		}
		selected = append(selected, ext)
	}
	return selected
}

// ResolvedSubcommand binds an extension's structured subcommand to the
// extension and flat command it routes to.
type ResolvedSubcommand struct {
	Extension     *ExtensionDescription
	Group         string
	Subcommand    string
	Description   string
	GroupDesc     string
	CommandName   string                    // flat command name within Extension.Jobs
	JobDefinition *JobDefinition            // resolved flat command
	GroupFlags    map[string]FlagDefinition // group-level shared flags inherited by every subcommand
	Subdef        SubcommandDefinition      // raw subcommand definition (interactive flag, etc.)
}

// EffectiveFlags returns the subcommand's effective flag surface, merging three
// layers in increasing precedence: the flat command target's flags, the group's
// shared flags, then the subcommand's own flags. A subcommand always overrides
// the group on a name collision, and the group overrides the flat command target.
func (r *ResolvedSubcommand) EffectiveFlags() map[string]FlagDefinition {
	var jobFlags map[string]FlagDefinition
	if r.JobDefinition != nil {
		jobFlags = r.JobDefinition.Flags
	}
	return MergeFlagLayers(jobFlags, r.GroupFlags, r.Subdef.Flags)
}

// CommandGroupNames returns the set of structured command-group names exposed
// by the given extensions. The result is nil when no extensions declare
// command groups.
func CommandGroupNames(extensions []*ExtensionDescription) map[string]bool {
	var names map[string]bool
	for _, ext := range extensions {
		for groupName := range ext.CommandGroups {
			if names == nil {
				names = make(map[string]bool)
			}
			names[groupName] = true
		}
	}
	return names
}

// GroupCarriedCommands returns the flat commands the subcommands of groups
// route to, nested subcommands included. A user types such a command as a
// subcommand of its group; typed directly, it fails. Help and completion both
// leave it out of their lists of top-level commands.
func GroupCarriedCommands(groups map[string]CommandGroupDefinition) map[string]bool {
	carried := map[string]bool{}
	var walk func(map[string]SubcommandDefinition)
	walk = func(subcommands map[string]SubcommandDefinition) {
		for _, subcommand := range subcommands {
			if subcommand.Command != "" {
				carried[subcommand.Command] = true
			}
			walk(subcommand.Subcommands)
		}
	}
	for _, group := range groups {
		walk(group.Subcommands)
	}
	return carried
}

// InteractiveOnlyCarrier returns the command path, `<group> <subcommand>`,
// that a user types to run command, when every subcommand of extensions that
// carries command (GroupCarriedCommands) dispatches interactive: such a command
// needs the terminal's streams and the host channel that interactive dispatch
// opens. A nested subcommand dispatches under the Interactive flag of its
// top-level subcommand. Of several carriers it returns the first path in
// sorted order. ok is false when no subcommand carries command, or one that
// carries it is not interactive.
func InteractiveOnlyCarrier(extensions []*ExtensionDescription, command string) (path string, ok bool) {
	var paths []string
	for _, ext := range extensions {
		for group, groupDef := range ext.CommandGroups {
			for name, sub := range groupDef.Subcommands {
				carrier, carried := carrierPath(sub, group+" "+name, command)
				if !carried {
					continue
				}
				if !sub.Interactive {
					return "", false
				}
				paths = append(paths, carrier)
			}
		}
	}
	if len(paths) == 0 {
		return "", false
	}
	sort.Strings(paths)
	return paths[0], true
}

// carrierPath returns the path of the first subcommand, in sorted order, at or
// below sub (whose own path is path) that names command.
func carrierPath(sub SubcommandDefinition, path, command string) (string, bool) {
	if sub.Command == command {
		return path, true
	}
	names := make([]string, 0, len(sub.Subcommands))
	for name := range sub.Subcommands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if carrier, ok := carrierPath(sub.Subcommands[name], path+" "+name, command); ok {
			return carrier, true
		}
	}
	return "", false
}

// CommandSubcommandOwners returns, sorted, the names of every extension with
// an executable definition for the given command-group subcommand. Multiple
// owners make dispatch first-match-wins across independently released
// manifests, while extensions contributing different subcommands remain
// composable.
func CommandSubcommandOwners(extensions []*ExtensionDescription, group, sub string) []string {
	var owners []string
	for _, ext := range extensions {
		groupDef, ok := ext.CommandGroups[group]
		if !ok {
			continue
		}
		subdef, ok := groupDef.Subcommands[sub]
		if !ok || ext.Jobs[subdef.Command] == nil {
			continue
		}
		owners = append(owners, ext.Name)
	}
	sort.Strings(owners)
	return owners
}

// ResolveSubcommand returns the resolved subcommand binding for `<group> <sub>`.
// It searches all extensions and returns the first match. When the group exists
// but the subcommand does not, the second return reports the group with empty
// Subdef so callers can produce a "did you mean" style error.
func ResolveSubcommand(extensions []*ExtensionDescription, group, sub string) (*ResolvedSubcommand, *ExtensionDescription) {
	if group == "" {
		return nil, nil
	}
	var groupOwner *ExtensionDescription
	for _, ext := range extensions {
		groupDef, ok := ext.CommandGroups[group]
		if !ok {
			continue
		}
		if groupOwner == nil {
			groupOwner = ext
		}
		if sub == "" {
			continue
		}
		subdef, ok := groupDef.Subcommands[sub]
		if !ok {
			continue
		}
		jobDef := ext.Jobs[subdef.Command]
		if jobDef == nil {
			continue
		}
		return &ResolvedSubcommand{
			Extension:     ext,
			Group:         group,
			Subcommand:    sub,
			Description:   subdef.Description,
			GroupDesc:     groupDef.Description,
			CommandName:   subdef.Command,
			JobDefinition: jobDef,
			GroupFlags:    groupDef.Flags,
			Subdef:        subdef,
		}, ext
	}
	return nil, groupOwner
}

// LookupSubcommand returns the owning extension and raw SubcommandDefinition for
// `<group> <sub>`, searching all extensions. Unlike ResolveSubcommand it does
// not require the target flat command to exist, so it can render help for a
// subcommand whose implementation is missing or which is only a nested-
// subcommand parent. ok is false when the group or subcommand is unknown.
func LookupSubcommand(extensions []*ExtensionDescription, group, sub string) (*ExtensionDescription, SubcommandDefinition, bool) {
	for _, ext := range extensions {
		groupDef, ok := ext.CommandGroups[group]
		if !ok {
			continue
		}
		subdef, ok := groupDef.Subcommands[sub]
		if !ok {
			continue
		}
		return ext, subdef, true
	}
	return nil, SubcommandDefinition{}, false
}

// GroupDefaultSubcommand returns the subcommand `putnami <group>` runs when no
// subcommand word follows: the default declared by the first extension that
// owns the group and declares one, the same first-match order ResolveSubcommand
// uses. It returns "" when no owner declares a default or the declared default
// does not resolve to an executable subcommand, so the caller prints the group
// help as it does for a group without a default.
func GroupDefaultSubcommand(extensions []*ExtensionDescription, group string) string {
	for _, ext := range extensions {
		groupDef, ok := ext.CommandGroups[group]
		if !ok || groupDef.Default == "" {
			continue
		}
		if resolved, _ := ResolveSubcommand(extensions, group, groupDef.Default); resolved == nil {
			return ""
		}
		return groupDef.Default
	}
	return ""
}

// ResolveGroupInvocation returns the subcommand `putnami <group> <word>` runs,
// where word is the first word after the group, or "" when none follows. No
// word runs the group default (GroupDefaultSubcommand). A word that names a
// subcommand of the group runs that subcommand: a declared subcommand wins, so
// a project named like one is addressed by its id. Any other word runs the
// default when the default declares positionals, and positional reports that
// the word is the default's first argument rather than a subcommand. Otherwise
// it returns the word unchanged, so the caller reports an unknown subcommand.
func ResolveGroupInvocation(extensions []*ExtensionDescription, group, word string) (sub string, positional bool) {
	if word == "" {
		return GroupDefaultSubcommand(extensions, group), false
	}
	if _, _, ok := LookupSubcommand(extensions, group, word); ok {
		return word, false
	}
	if def := GroupPositionalDefault(extensions, group); def != nil {
		return def.Subcommand, true
	}
	return word, false
}

// GroupPositionalDefault returns the group default (GroupDefaultSubcommand)
// when it declares positionals, the subcommand that `putnami <group> <word>`
// runs for a word that names no subcommand of the group, and nil otherwise.
func GroupPositionalDefault(extensions []*ExtensionDescription, group string) *ResolvedSubcommand {
	def := GroupDefaultSubcommand(extensions, group)
	if def == "" {
		return nil
	}
	resolved, _ := ResolveSubcommand(extensions, group, def)
	if resolved == nil || len(resolved.Subdef.Positionals) == 0 {
		return nil
	}
	return resolved
}

// GroupInvocationCommand returns the flat command `putnami <group> <word>`
// runs (ResolveGroupInvocation), or "" when the invocation resolves to no
// executable subcommand. Its traits are the run's: whether it reads its
// ancestry, and whether it watches by default and owns --no-watch.
func GroupInvocationCommand(extensions []*ExtensionDescription, group, word string) string {
	sub, _ := ResolveGroupInvocation(extensions, group, word)
	if resolved, _ := ResolveSubcommand(extensions, group, sub); resolved != nil {
		return resolved.CommandName
	}
	return ""
}

// ListSubcommands returns the sorted subcommand names a group exposes across
// all extensions (de-duplicated). Useful for help and error messages.
func ListSubcommands(extensions []*ExtensionDescription, group string) []string {
	seen := make(map[string]struct{})
	for _, ext := range extensions {
		groupDef, ok := ext.CommandGroups[group]
		if !ok {
			continue
		}
		for sub := range groupDef.Subcommands {
			seen[sub] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for sub := range seen {
		out = append(out, sub)
	}
	sort.Strings(out)
	return out
}

// installedRemediation is appended to skew warnings for extensions loaded from
// the installed lock-pinned layout (.putnami/bin/extensions/…), where the fix
// is usually one command away. The text lives in the model package because
// SkippedProviderCause quotes the same remediation.
const installedRemediation = model.InstalledRemediation

// warnedOnce dedupes discovery warnings per process: discovery runs from many
// call sites (help, completion, planning, MCP), so a skipped or adapted
// extension would otherwise warn ~5× per invocation. Keyed by manifest path +
// reason.
var warnedOnce sync.Map

func warnOnce(key, msg string, args ...any) {
	if _, loaded := warnedOnce.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	slog.Warn(msg, args...)
}

// applyVocabularyTraits runs the vocabulary pass over the loaded extensions
// and warns, once per process, about each declaration the merge set aside: a
// verb two vocabularies declare.
func applyVocabularyTraits(extensions []*ExtensionDescription) {
	for _, warning := range ApplyVocabularyTraits(extensions) {
		warnOnce("vocabulary\x00"+warning, "verb vocabulary declaration set aside", "reason", warning)
	}
}

func tryLoadExtension(absPath, refName string, installed bool) (*ExtensionDescription, *SkippedExtension) {
	manifestPath := filepath.Join(absPath, "putnami.extension.json")
	// Distinguish "no manifest" (normal, the dir simply isn't an extension)
	// from "manifest exists but failed to load" (a real error the user needs
	// to see, otherwise the extension silently disappears from the workspace).
	if _, statErr := os.Stat(manifestPath); os.IsNotExist(statErr) {
		return nil, nil
	}
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		skip := &SkippedExtension{Ref: refName, Path: absPath, Reason: err}
		skip.Name = readPutnamiRCName(absPath)
		if skip.Name == "" {
			skip.Name = refName
		}
		warnArgs := []any{"path", manifestPath, "ref", refName, "error", err}
		if installed {
			warnArgs = append(warnArgs, "remediation", installedRemediation)
		}
		warnOnce(manifestPath+"\x00"+err.Error(),
			"extension manifest cannot be loaded; extension skipped", warnArgs...)
		return nil, skip
	}

	// No adaptation branch: since CLI contract 3 a manifest that predates the
	// contract does not load at all (LoadManifest), so a loaded manifest is
	// exactly what its author wrote.
	ext := Resolve(manifest, absPath)

	// Determine canonical name. Priority:
	// 1. Manifest name field
	// 2. putnami.json name (native projects)
	// 3. Reference name (fallback)
	// The version comes from the manifest alone.
	if ext.Name == "" {
		rcName := readPutnamiRCName(absPath)
		if rcName != "" {
			ext.Name = rcName
		}
	}
	if ext.Name == "" {
		ext.Name = refName
	}

	// Propagate the extension name to all job definitions.
	// Note: ExtensionPath is set later by DiscoverExtensions once RelPath is known.
	for _, job := range ext.Jobs {
		if job.ExtensionName == "" {
			job.ExtensionName = ext.Name
		}
	}

	return ext, nil
}

func readPutnamiRCName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "putnami.json"))
	if err != nil {
		return ""
	}
	type rc struct {
		Name string `json:"name"`
	}
	var r rc
	if err := json.Unmarshal(data, &r); err != nil {
		return ""
	}
	return r.Name
}

// installedPackagesDirName is the name of the directory a package manager
// installs into. It is no source of extensions; a hosted run refuses a path
// extension that lies inside one (inInstalledPackages).
const installedPackagesDirName = "node_modules"
