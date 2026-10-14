package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// Extension dependencies (ADR 0066).
//
// The extensions putnami.workspace.json declares are the roots of a graph. The
// graph also holds every registry extension (@scope/name) that the manifest of
// a graph member names in extensionDependencies, transitively: a dependency.
// Only the workspace file satisfies a dependency, by name or by a path whose
// manifest has that name. The lock pins a dependency with an ordinary entry,
// and the workspace file never receives one.
//
// The CLI follows the edges of a member loaded from the artifact store or from
// a path the workspace declares, and of a declared name the lock pins wherever
// it loaded from. It does not follow the edges of any other extension loaded
// from node_modules: the package manager installed it and owns them.

// portableChannel matches the channel alphabet every ecosystem accepts
// (extproto.PortableChannelPattern).
var portableChannel = regexp.MustCompile(extproto.PortableChannelPattern)

// versionTag matches a semantic version, with or without a leading v, and
// whatever pre-release or build suffix follows it.
var versionTag = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([.+-].*)?$`)

// latestRequest is the request that resolves on the registry's newest release.
const latestRequest = "latest"

// InPortableChannelAlphabet reports whether name is in the channel alphabet
// every ecosystem accepts.
func InPortableChannelAlphabet(name string) bool {
	return portableChannel.MatchString(name)
}

// IsVersionTag reports whether s is a semantic version, with or without a
// leading v.
func IsVersionTag(s string) bool {
	return versionTag.MatchString(s)
}

// IsChannelRequest reports whether request names a channel: it is in the
// portable channel alphabet and is neither a semantic version, with or without
// a leading v, nor a range. "latest" is a channel.
func IsChannelRequest(request string) bool {
	if !InPortableChannelAlphabet(request) || IsVersionTag(request) {
		return false
	}
	if request == latestRequest {
		return true
	}
	_, err := ParseConstraint(request)
	return err != nil
}

// IsExplicitChannel reports whether request names a channel other than latest.
// When a run asks for one, every dependency below the requested extension
// resolves on it.
func IsExplicitChannel(request string) bool {
	return request != latestRequest && IsChannelRequest(request)
}

// IsRangeConstraint reports whether constraint parses as a semantic version
// range a resolved version can miss: an exact version included, and "", "*"
// and "latest" excluded.
func IsRangeConstraint(constraint string) bool {
	parsed, err := ParseConstraint(constraint)
	return err == nil && !parsed.IsLatest
}

// SatisfiesRange reports whether version satisfies the range constraint
// (IsRangeConstraint). A version that does not parse satisfies no range.
func SatisfiesRange(version, constraint string) bool {
	parsed, err := ParseConstraint(constraint)
	if err != nil {
		return false
	}
	v, err := ParseVersion(version)
	if err != nil || !isPinnableVersion(version) {
		return false
	}
	return parsed.Match(v)
}

// DependencyNames returns the registry extensions deps names, sorted
// (extproto.RegistryDependencies). Any other name is left to the package
// manager that installed the extension.
func DependencyNames(deps Dependencies) []string {
	return extproto.RegistryDependencies(&Manifest{ExtensionDeps: deps})
}

// GraphDependency is an extension of the workspace's graph that
// putnami.workspace.json does not declare.
type GraphDependency struct {
	// Name is its registry name.
	Name string
	// RequiredBy are the graph members whose manifest names it, in the order
	// the walk reached them. The first one is the dependent it resolved for.
	RequiredBy []string
	// Loaded reports whether its manifest was read and its edges followed.
	Loaded bool
}

// DependencyPath returns the members on the path from a root to name, the
// root first and name last, or nil when name is not one of deps. Each step
// goes to the first dependent, so the path is a shortest one.
func DependencyPath(deps []GraphDependency, name string) []string {
	byName := make(map[string]GraphDependency, len(deps))
	for _, dep := range deps {
		byName[dep.Name] = dep
	}
	if _, ok := byName[name]; !ok {
		return nil
	}
	path := []string{name}
	for current := name; ; {
		dep, ok := byName[current]
		if !ok || len(dep.RequiredBy) == 0 || slices.Contains(path, dep.RequiredBy[0]) {
			break
		}
		current = dep.RequiredBy[0]
		path = append(path, current)
	}
	slices.Reverse(path)
	return path
}

// graphMember is a graph member whose manifest was read: its name and the
// registry dependencies its manifest names, sorted.
type graphMember struct {
	name string
	deps []string
}

// walkGraph walks the dependencies of roots breadth-first: level by level, the
// members of a level by name, and the dependencies of a member by name. A
// declared name satisfies its dependency and is not walked. resolve runs once
// per dependency, at the first level that reaches it, with the first
// dependent. It returns the dependency as a member, or false when its manifest
// was not read, which ends the walk below it.
func walkGraph(roots []graphMember, declared map[string]bool, resolve func(name, requiredBy string) (graphMember, bool)) []GraphDependency {
	index := make(map[string]int)
	var deps []GraphDependency
	level := sortedMembers(roots)
	for len(level) > 0 {
		var next []graphMember
		for _, member := range level {
			for _, name := range member.deps {
				if declared[name] {
					continue
				}
				if i, ok := index[name]; ok {
					if !slices.Contains(deps[i].RequiredBy, member.name) {
						deps[i].RequiredBy = append(deps[i].RequiredBy, member.name)
					}
					continue
				}
				index[name] = len(deps)
				deps = append(deps, GraphDependency{Name: name, RequiredBy: []string{member.name}})
				if resolved, ok := resolve(name, member.name); ok {
					deps[index[name]].Loaded = true
					next = append(next, resolved)
				}
			}
		}
		level = sortedMembers(next)
	}
	return deps
}

func sortedMembers(members []graphMember) []graphMember {
	sorted := slices.Clone(members)
	slices.SortStableFunc(sorted, func(a, b graphMember) int {
		switch {
		case a.name < b.name:
			return -1
		case a.name > b.name:
			return 1
		}
		return 0
	})
	return sorted
}

// GlobalOnlyExtensions returns the extension keys the global configuration
// (~/.putnami/config.json) declares and putnami.workspace.json does not. Such
// a key is not a root of the graph, and when it names a graph member the
// graph ignores it.
func GlobalOnlyExtensions(workspaceRoot string) map[string]bool {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	global := readExtensionKeys(filepath.Join(home, wsproto.GlobalConfigDir, wsproto.GlobalConfigFilename))
	if len(global) == 0 {
		return nil
	}
	var workspace map[string]string
	if workspaceRoot != "" {
		workspace = readExtensionKeys(wsproto.ResolveFile(workspaceRoot, wsproto.WorkspaceConfigFilename))
	}
	only := make(map[string]bool)
	for key := range global {
		if _, declared := workspace[key]; !declared {
			only[key] = true
		}
	}
	return only
}

// GlobalConfigDisplay names the global configuration in a message.
var GlobalConfigDisplay = filepath.ToSlash(filepath.Join("~", wsproto.GlobalConfigDir, wsproto.GlobalConfigFilename))

// readExtensionKeys returns the `extensions` entries of one configuration
// file, or nil when it is absent or unreadable, as wsproto.Load reads it.
func readExtensionKeys(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var config struct {
		Extensions wsproto.ExtensionsConfig `json:"extensions"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil
	}
	return config.Extensions.List
}

// errDependencyNotPinned is why a dependency the lock does not pin is not
// loaded or restored: nothing but an explicit install resolves one.
var errDependencyNotPinned = fmt.Errorf("%s does not pin it; run `putnami install`", lockfile.LockFilename)

// errDependencyNotInstalled is why a pinned dependency whose build is absent
// is not loaded.
var errDependencyNotInstalled = errors.New("the build the lock pins is not installed; run `putnami install`")

// errDependencyProvider is why a dependency the workspace does not declare
// serves no reserved provider command (hostedProviderCommands).
var errDependencyProvider = fmt.Errorf("an extension serves a provider only when %s declares it", wsproto.WorkspaceConfigFilename)

// dependencyNotLoaded names the dependent of a dependency that did not load.
func dependencyNotLoaded(name, requiredBy string, reason error) error {
	return fmt.Errorf("%s requires %s: %w", requiredBy, name, reason)
}

// IsDependencyNotPinned reports whether err says that the lock does not pin a
// dependency.
func IsDependencyNotPinned(err error) bool {
	return errors.Is(err, errDependencyNotPinned)
}

// withoutDependencyProviders removes every reserved provider command from ext,
// a dependency the workspace does not declare, and records the dependent.
func withoutDependencyProviders(ext *ExtensionDescription, requiredBy string) []RemovedCapability {
	removed := withoutProviderCapabilities(ext)
	for i := range removed {
		removed[i].RequiredBy = requiredBy
	}
	return removed
}

// checkFollowedContract applies the dependency rung to ext, a manifest whose
// edges the CLI follows because it loaded from the artifact store or from a
// declared path (extproto.CheckDependencyContract). The stamp is read from the
// manifest itself, only when it names a registry dependency.
func checkFollowedContract(ext *ExtensionDescription) error {
	if len(DependencyNames(ext.ExtDeps)) == 0 {
		return nil
	}
	path := filepath.Join(ext.Path, ManifestFilename)
	m := &Manifest{CLIContract: readManifestContract(path), ExtensionDeps: ext.ExtDeps}
	return extproto.CheckDependencyContract(path, m)
}

// readManifestContract returns the cliContract stamp of the manifest at path,
// 0 when it is absent or unreadable.
func readManifestContract(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var stamp struct {
		CLIContract int `json:"cliContract"`
	}
	if err := json.Unmarshal(data, &stamp); err != nil {
		return 0
	}
	return stamp.CLIContract
}

// dependencyDiscovery is discovery's dependency step: it runs after the
// declared extensions loaded and before the root package.json development
// dependencies, which skip every name it records in seen.
type dependencyDiscovery struct {
	workspaceRoot string
	cfg           *wsproto.Config
	lockFile      *lockfile.LockFile
	hosted        bool
	// installed holds the declared extensions loaded from the install layout:
	// the stable link or the artifact directory of the lock pin.
	installed map[*ExtensionDescription]bool
	// projects holds the workspace projects discovery loaded because the
	// workspace config does not pin their name.
	projects map[*ExtensionDescription]bool
}

// run loads the dependencies of the declared extensions in extensions and
// returns the extensions, the dependencies, the skips and the provider
// capabilities it removed. A workspace whose followed extensions name no
// registry dependency is returned unchanged, without a read.
//
// A dependency loads only from the install layout, under its lock pin.
// Whatever else loaded under its name before this step is removed, whether
// the build loads or not: a workspace project, which the build records in
// PinnedOver, or the extension of a global-configuration key, with one warning
// that names the global configuration. A dependency that does not load is
// returned in skipped, naming its dependent.
func (d *dependencyDiscovery) run(extensions []*ExtensionDescription, seen map[string]bool) ([]*ExtensionDescription, []GraphDependency, []SkippedExtension, []RemovedCapability) {
	candidates := d.roots(extensions, nil)
	if !namesDependencies(candidates) {
		return extensions, nil, nil, nil
	}
	globalOnly := GlobalOnlyExtensions(d.workspaceRoot)
	roots := d.roots(extensions, globalOnly)

	var skipped []SkippedExtension
	var removed []RemovedCapability
	declared := make(map[string]bool, len(roots))
	var members []graphMember
	for _, root := range roots {
		for _, name := range root.declares {
			declared[name] = true
		}
		if root.ext == nil || !root.followed {
			continue
		}
		if root.floor {
			if err := checkFollowedContract(root.ext); err != nil {
				extensions = slices.DeleteFunc(extensions, func(e *ExtensionDescription) bool { return e == root.ext })
				skipped = append(skipped, SkippedExtension{Ref: root.key, Name: root.ext.Name, Path: root.ext.Path, Version: root.ext.Version, Reason: err})
				warnOnce("dependency-floor:"+root.ext.Path, "extension manifest names extension dependencies under an older contract; extension skipped",
					"extension", root.ext.Name, "error", err)
				continue
			}
		}
		members = append(members, graphMember{name: root.ext.Name, deps: DependencyNames(root.ext.ExtDeps)})
	}

	deps := walkGraph(members, declared, func(name, requiredBy string) (graphMember, bool) {
		var replaced string
		fromGlobalKey := globalOnly[name]
		if existing := FindExtensionByName(extensions, name); existing != nil {
			if d.projects[existing] {
				replaced = existing.RelPath
			} else {
				fromGlobalKey = true
			}
			extensions = slices.DeleteFunc(extensions, func(e *ExtensionDescription) bool { return e == existing })
		}
		seen[name] = true
		if fromGlobalKey {
			warnOnce("dependency-global-key:"+d.workspaceRoot+":"+name,
				"the global configuration declares an extension the workspace graph requires as a dependency; the global key is ignored",
				"extension", name, "requiredBy", requiredBy, "config", GlobalConfigDisplay)
		}
		ext, skip := d.load(name, requiredBy)
		if skip != nil {
			skipped = append(skipped, *skip)
			warnOnce("dependency-unloaded:"+d.workspaceRoot+":"+name+"\x00"+skip.Reason.Error(),
				"an extension dependency did not load", "extension", name, "error", skip.Reason)
			return graphMember{}, false
		}
		removed = append(removed, withoutDependencyProviders(ext, requiredBy)...)
		if replaced != "" {
			ext.PinnedOver = replaced
			for _, job := range ext.Jobs {
				if job.ExtensionPath == "" {
					job.ExtensionPath = replaced
				}
			}
		}
		extensions = append(extensions, ext)
		return graphMember{name: name, deps: DependencyNames(ext.ExtDeps)}, true
	})
	return extensions, deps, skipped, removed
}

// load loads the build the lock pins for the dependency name, or returns the
// skip that says why it did not load.
func (d *dependencyDiscovery) load(name, requiredBy string) (*ExtensionDescription, *SkippedExtension) {
	skipped := func(path, version string, reason error) *SkippedExtension {
		return &SkippedExtension{Ref: name, Name: name, Path: path, Version: version, Reason: dependencyNotLoaded(name, requiredBy, reason)}
	}
	var entry lockfile.LockEntry
	pinned := false
	if d.lockFile != nil {
		entry, pinned = d.lockFile.GetExtension(name)
	}
	if !pinned {
		return nil, skipped("", "", errDependencyNotPinned)
	}
	stable := layout.StableDir(d.workspaceRoot, layout.Extensions, name)
	ext, skip := tryLoadInstalledExtension(d.workspaceRoot, name, d.lockFile)
	switch {
	case skip != nil:
		return nil, skipped(skip.Path, entry.Version, skip.Reason)
	case ext == nil:
		return nil, skipped(stable, entry.Version, errDependencyNotInstalled)
	case ext.Name != name:
		return nil, skipped(ext.Path, entry.Version, fmt.Errorf("the installed manifest names %q", ext.Name))
	}
	if err := checkFollowedContract(ext); err != nil {
		return nil, skipped(ext.Path, entry.Version, err)
	}
	return ext, nil
}

// declaredRoot is one non-empty `extensions` key of the workspace and the
// extension discovery loaded for it, when one did.
type declaredRoot struct {
	key string
	ext *ExtensionDescription
	// declares are the names the key satisfies: the key itself, and the
	// manifest name of the extension it declares by path.
	declares []string
	// followed reports whether the CLI follows its edges, and floor whether
	// the dependency rung applies to its manifest (CheckDependencyContract).
	followed, floor bool
}

// roots returns the declared extensions in key order, leaving out every key
// in globalOnly. A key loads by path when an extension loaded from that path
// (a local source with that RelPath, or that absolute Path); its edges are
// followed and the rung applies. Any other key names an extension: its edges
// are followed when it loaded from the install layout, where the rung applies,
// or when the lock pins the key, wherever it loaded from.
func (d *dependencyDiscovery) roots(extensions []*ExtensionDescription, globalOnly map[string]bool) []declaredRoot {
	if d.cfg == nil {
		return nil
	}
	var roots []declaredRoot
	for _, key := range d.cfg.Extensions.Names() {
		if globalOnly[key] {
			continue
		}
		if ext := d.loadedByPath(extensions, key); ext != nil {
			roots = append(roots, declaredRoot{key: key, ext: ext, declares: []string{key, ext.Name}, followed: true, floor: true})
			continue
		}
		root := declaredRoot{key: key, declares: []string{key}}
		if ext := FindExtensionByName(extensions, key); ext != nil {
			root.ext = ext
			root.floor = d.installed[ext]
			root.followed = root.floor || d.locks(key)
		}
		roots = append(roots, root)
	}
	return roots
}

// loadedByPath returns the extension that key loaded as a path: an absolute
// path, or a workspace path a hosted run reads only from a path-shaped key.
func (d *dependencyDiscovery) loadedByPath(extensions []*ExtensionDescription, key string) *ExtensionDescription {
	if filepath.IsAbs(key) {
		for _, ext := range extensions {
			if ext.LocalSource && filepath.Clean(ext.Path) == filepath.Clean(key) {
				return ext
			}
		}
	}
	rel, ok := normalizeWorkspaceExtensionPath(key)
	if !ok || (d.hosted && !declaredByPath(key)) {
		return nil
	}
	for _, ext := range extensions {
		if ext.LocalSource && ext.RelPath == rel {
			return ext
		}
	}
	return nil
}

func (d *dependencyDiscovery) locks(name string) bool {
	if d.lockFile == nil {
		return false
	}
	_, ok := d.lockFile.GetExtension(name)
	return ok
}

func namesDependencies(roots []declaredRoot) bool {
	for _, root := range roots {
		if root.ext != nil && root.followed && len(DependencyNames(root.ext.ExtDeps)) > 0 {
			return true
		}
	}
	return false
}

// DependencyIssue is a dependency a graph walk did not read: the lock does not
// pin it, its build is not installed, its restore failed, or its manifest
// claims the dependency rung below ExtensionDependenciesContract.
type DependencyIssue struct {
	Name string
	// RequiredBy is the dependent the walk reached it from.
	RequiredBy string
	// Version is its lock pin, "" when the lock does not pin it.
	Version string
	// Reason names the dependent and the cause.
	Reason error
}

// GraphWalk is the outcome of WalkWorkspaceGraph.
type GraphWalk struct {
	// Declared holds the names putnami.workspace.json declares: each key, and
	// the manifest name of each extension a key declares by path.
	Declared map[string]bool
	// Dependencies are the graph's dependencies in walk order.
	Dependencies []GraphDependency
	// Issues are the dependencies the walk did not read, in walk order.
	Issues []DependencyIssue
}

// Dependency returns the dependency name, and whether the graph holds it.
func (w GraphWalk) Dependency(name string) (GraphDependency, bool) {
	for _, dep := range w.Dependencies {
		if dep.Name == name {
			return dep, true
		}
	}
	return GraphDependency{}, false
}

// WalkOptions tunes WalkWorkspaceGraph.
type WalkOptions struct {
	// Without leaves one declaration out of the roots: an `extensions` key, or
	// the manifest name a key declares by path.
	Without string
	// Extra adds one root, read like an `extensions` key: the extension a
	// removal is about, which the workspace file may no longer declare.
	Extra string
	// Restore, when set, materializes a dependency the lock pins before its
	// manifest is read. Its error is an issue, and the walk does not go below
	// that dependency. A dependency the lock does not pin is never restored.
	Restore func(name string, entry lockfile.LockEntry) error
}

// WalkWorkspaceGraph walks the dependencies of the extensions
// putnami.workspace.json declares from the manifests on disk, following the
// edges discovery follows. A root is read where discovery loads it; a
// dependency is read from its stable link or the directory of its lock pin.
// A global-configuration key is not a root. The walk never resolves a
// dependency the lock does not pin: that one is an issue that names its
// dependent and `putnami install`.
func WalkWorkspaceGraph(workspaceRoot string, cfg *wsproto.Config, lf *lockfile.LockFile, opts WalkOptions) GraphWalk {
	walk := GraphWalk{Declared: map[string]bool{}}
	var keys []string
	if cfg != nil {
		keys = cfg.Extensions.Names()
	}
	extra := opts.Extra != "" && !slices.Contains(keys, opts.Extra)
	if extra {
		keys = append(keys, opts.Extra)
	}
	if len(keys) == 0 {
		return walk
	}
	hosted := runcredential.Hosted()
	type root struct {
		key      string
		at       rootLocation
		manifest rawManifest
	}
	var roots []root
	dependent := false
	for _, key := range keys {
		r := root{key: key, at: locateRoot(workspaceRoot, key, lf, hosted)}
		if r.at.dir != "" {
			r.manifest = readRawManifest(r.at.dir, key)
		}
		dependent = dependent || (r.at.followed && len(r.manifest.deps) > 0)
		roots = append(roots, r)
	}
	var globalOnly map[string]bool
	if dependent {
		globalOnly = GlobalOnlyExtensions(workspaceRoot)
	}
	var members []graphMember
	for _, r := range roots {
		if (globalOnly[r.key] && !(extra && r.key == opts.Extra)) || r.key == opts.Without {
			continue
		}
		name := r.key
		if r.at.byPath && r.manifest.read {
			if r.manifest.name == opts.Without {
				continue
			}
			name = r.manifest.name
			walk.Declared[name] = true
		}
		walk.Declared[r.key] = true
		if !r.at.followed || !r.manifest.read || len(r.manifest.deps) == 0 {
			continue
		}
		if r.at.floor && r.manifest.floorErr() != nil {
			continue
		}
		members = append(members, graphMember{name: name, deps: r.manifest.deps})
	}
	if !dependent {
		return walk
	}

	walk.Dependencies = walkGraph(members, walk.Declared, func(name, requiredBy string) (graphMember, bool) {
		issue := func(version string, reason error) (graphMember, bool) {
			walk.Issues = append(walk.Issues, DependencyIssue{Name: name, RequiredBy: requiredBy, Version: version,
				Reason: dependencyNotLoaded(name, requiredBy, reason)})
			return graphMember{}, false
		}
		var entry lockfile.LockEntry
		pinned := false
		if lf != nil {
			entry, pinned = lf.GetExtension(name)
		}
		if !pinned {
			return issue("", errDependencyNotPinned)
		}
		if opts.Restore != nil {
			if err := opts.Restore(name, entry); err != nil {
				return issue(entry.Version, err)
			}
		}
		dir := InstalledDependencyDir(workspaceRoot, name, entry.Version)
		if dir == "" {
			return issue(entry.Version, errDependencyNotInstalled)
		}
		manifest := readRawManifest(dir, name)
		switch {
		case !manifest.read:
			return issue(entry.Version, errDependencyNotInstalled)
		case manifest.floorErr() != nil:
			return issue(entry.Version, manifest.floorErr())
		}
		return graphMember{name: name, deps: manifest.deps}, true
	})
	return walk
}

// InstalledDependencyDir returns the directory discovery loads the dependency
// name from: its stable link, or the artifact directory of version. It
// returns "" when neither holds a manifest.
func InstalledDependencyDir(workspaceRoot, name, version string) string {
	if dir := layout.StableDir(workspaceRoot, layout.Extensions, name); manifestResolves(dir, ManifestFilename) {
		return dir
	}
	if version == "" {
		return ""
	}
	dir := layout.ArtifactDir(workspaceRoot, layout.Extensions, name, version)
	if _, err := os.Stat(filepath.Join(dir, ManifestFilename)); err != nil {
		return ""
	}
	return dir
}

// rootLocation is where discovery loads a declared key from.
type rootLocation struct {
	dir string
	// byPath reports a key that loaded as a path, so it declares the name its
	// manifest has.
	byPath bool
	// followed reports whether the CLI follows the root's edges, and floor
	// whether the dependency rung applies to its manifest.
	followed, floor bool
}

// locateRoot returns where discovery loads the declared key from, in its
// probe order: an absolute path, a workspace path, node_modules, then the
// install layout. Its edges are followed except from node_modules, where the
// lock must pin the key. The rung applies except from node_modules.
func locateRoot(workspaceRoot, key string, lf *lockfile.LockFile, hosted bool) rootLocation {
	holds := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, ManifestFilename))
		return err == nil
	}
	if filepath.IsAbs(key) && holds(key) {
		return rootLocation{dir: key, byPath: true, followed: true, floor: true}
	}
	if filepath.VolumeName(key) != "" {
		return rootLocation{}
	}
	if rel, ok := normalizeWorkspaceExtensionPath(key); ok && (!hosted || declaredByPath(key)) {
		if dir := filepath.Join(workspaceRoot, rel); holds(dir) {
			return rootLocation{dir: dir, byPath: true, followed: true, floor: true}
		}
	}
	var entry lockfile.LockEntry
	pinned := false
	if lf != nil {
		entry, pinned = lf.GetExtension(key)
	}
	if !hosted {
		if dir := installedPackageDir(workspaceRoot, key); holds(dir) {
			return rootLocation{dir: dir, followed: pinned}
		}
	}
	if dir := InstalledDependencyDir(workspaceRoot, key, entry.Version); dir != "" {
		return rootLocation{dir: dir, followed: true, floor: true}
	}
	return rootLocation{}
}

// rawManifest is what a graph walk reads from a manifest without loading it.
type rawManifest struct {
	read     bool
	path     string
	name     string
	contract int
	declared Dependencies
	deps     []string
}

// floorErr is the dependency rung's refusal of the manifest
// (extproto.CheckDependencyContract), nil when it holds or names no registry
// dependency.
func (m rawManifest) floorErr() error {
	if len(m.deps) == 0 {
		return nil
	}
	return extproto.CheckDependencyContract(m.path, &Manifest{CLIContract: m.contract, ExtensionDeps: m.declared})
}

// readRawManifest reads the name, the stamp and the registry dependencies of
// the manifest in dir. The name falls back as discovery's does: package.json,
// then putnami.json, then ref.
func readRawManifest(dir, ref string) rawManifest {
	path := filepath.Join(dir, ManifestFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return rawManifest{}
	}
	var m struct {
		Name          string       `json:"name"`
		CLIContract   int          `json:"cliContract"`
		ExtensionDeps Dependencies `json:"extensionDependencies"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return rawManifest{}
	}
	name := m.Name
	if name == "" {
		name, _ = readPackageNameVersion(dir)
	}
	if name == "" {
		name = readPutnamiRCName(dir)
	}
	if name == "" {
		name = ref
	}
	return rawManifest{read: true, path: path, name: name, contract: m.CLIContract,
		declared: m.ExtensionDeps, deps: DependencyNames(m.ExtensionDeps)}
}

// GraphManifest is a graph member's manifest as an install reads it.
type GraphManifest struct {
	// Name is the manifest name, with discovery's fallbacks.
	Name string
	// Dependencies maps each registry dependency the manifest names to its
	// constraint ("" when it states none).
	Dependencies map[string]string
	// FloorErr is the dependency rung's refusal of the manifest
	// (extproto.CheckDependencyContract), nil when the rung holds.
	FloorErr error
}

// ReadGraphManifest reads the manifest in dir, an installed build or a
// declared directory, and reports whether it holds one.
func ReadGraphManifest(dir, ref string) (GraphManifest, bool) {
	m := readRawManifest(dir, ref)
	if !m.read {
		return GraphManifest{}, false
	}
	return GraphManifest{Name: m.name, Dependencies: m.edges(), FloorErr: m.floorErr()}, true
}

// edges maps each registry dependency to its constraint.
func (m rawManifest) edges() map[string]string {
	if len(m.deps) == 0 {
		return nil
	}
	edges := make(map[string]string, len(m.deps))
	for _, name := range m.deps {
		edges[name] = m.declared.List[name]
	}
	return edges
}

// DeclaredRoot is an extension putnami.workspace.json declares, as a graph
// install reads it from disk without installing it.
type DeclaredRoot struct {
	GraphManifest
	// ByPath reports a key that loads as a path: it declares Name too, and its
	// version is not checked against a constraint.
	ByPath bool
	// Followed reports whether the CLI follows its edges (ADR 0066 point 3).
	Followed bool
}

// ReadDeclaredRoot reads the declared key where discovery loads it from, and
// reports whether a manifest was found.
func ReadDeclaredRoot(workspaceRoot, key string, lf *lockfile.LockFile) (DeclaredRoot, bool) {
	at := locateRoot(workspaceRoot, key, lf, runcredential.Hosted())
	if at.dir == "" {
		return DeclaredRoot{}, false
	}
	m := readRawManifest(at.dir, key)
	if !m.read {
		return DeclaredRoot{}, false
	}
	root := DeclaredRoot{
		GraphManifest: GraphManifest{Name: m.name, Dependencies: m.edges()},
		ByPath:        at.byPath,
		Followed:      at.followed,
	}
	if at.floor {
		root.FloorErr = m.floorErr()
	}
	return root, true
}
