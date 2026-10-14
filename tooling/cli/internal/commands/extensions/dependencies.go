package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// Extension dependencies (ADR 0066). The extensions putnami.workspace.json
// declares are the roots of a graph; every registry extension a member's
// manifest names in extensionDependencies is a dependency. An install resolves
// the graph level by level and pins each dependency with an ordinary lock
// entry. It never writes the workspace file for a dependency.

// graphOps reads from disk what a graph install needs. Only the workspace
// extension ops carry one: templates have no dependencies, and the user scope
// takes none.
type graphOps struct {
	// root reads a declared key where discovery loads it.
	root func(key string, lf *lockfile.LockFile) (extension.DeclaredRoot, bool)
	// globalOnly returns the keys only the global configuration declares.
	globalOnly func() map[string]bool
	// walk walks the workspace graph from the manifests on disk.
	walk func(cfg *wsproto.Config, lf *lockfile.LockFile, opts extension.WalkOptions) extension.GraphWalk
}

func workspaceGraphOps(wsRoot string) *graphOps {
	return &graphOps{
		root: func(key string, lf *lockfile.LockFile) (extension.DeclaredRoot, bool) {
			return extension.ReadDeclaredRoot(wsRoot, key, lf)
		},
		globalOnly: func() map[string]bool { return extension.GlobalOnlyExtensions(wsRoot) },
		walk: func(cfg *wsproto.Config, lf *lockfile.LockFile, opts extension.WalkOptions) extension.GraphWalk {
			return extension.WalkWorkspaceGraph(wsRoot, cfg, lf, opts)
		},
	}
}

// withGraphManifest records in outcome the manifest an install placed in dir.
func withGraphManifest(outcome *artifactInstallOutcome, dir, name string) {
	if dir == "" {
		return
	}
	if manifest, ok := extension.ReadGraphManifest(dir, name); ok {
		outcome.Manifest = &manifest
	}
}

// graphMember is a graph member whose manifest was read.
type graphMember struct {
	name string
	// deps maps each registry dependency to its constraint.
	deps map[string]string
	// channel is the explicit channel its dependencies resolve on, "" when
	// none flows down to it.
	channel string
	// fresh reports that its dependencies resolve again, whatever the lock
	// holds.
	fresh bool
}

// graphEdge is one extensionDependencies entry of a graph member.
type graphEdge struct {
	from, to, constraint string
}

// graphNode is a dependency: a graph member the workspace does not declare.
type graphNode struct {
	name string
	// requiredBy are its dependents, the first one the dependent it resolved
	// for.
	requiredBy []string
	request    string
	channel    string
	fresh      bool
	// known are the constraints known when it resolved.
	known []graphEdge
	item  artifactInstallWork
}

// installGraph is the dependency graph of one install or update.
type installGraph struct {
	ops      artifactOps
	lockFile *lockfile.LockFile
	// persist reports a run that writes the lock: only such a run resolves a
	// dependency the lock does not pin, or moves one.
	persist bool
	// declared holds the names the workspace satisfies.
	declared map[string]bool
	// versions maps a root the workspace declares by name to its version.
	versions map[string]string
	nodes    map[string]*graphNode
	order    []*graphNode
	edges    []graphEdge
	// rootErrs maps a root to the check of the graph it failed.
	rootErrs map[string]error
}

func newInstallGraph(ops artifactOps, lockFile *lockfile.LockFile, persist bool) *installGraph {
	return &installGraph{
		ops: ops, lockFile: lockFile, persist: persist,
		declared: map[string]bool{}, versions: map[string]string{},
		nodes: map[string]*graphNode{}, rootErrs: map[string]error{},
	}
}

// resolve resolves the dependencies of roots breadth-first. Every node of a
// level resolves, and its manifest is read, before any node of the next level
// resolves. Within a level, a member under an explicit channel comes first,
// then the members by name, then their dependencies by name. A node resolves
// once, at the first level that reaches it, on the request of the first
// dependent in that order. A constraint found later is only checked.
func (g *installGraph) resolve(ctx context.Context, roots []graphMember) {
	level := sortGraphLevel(roots)
	for len(level) > 0 {
		reached := g.reach(level)
		g.prepare(reached)
		work := make([]artifactInstallWork, len(reached))
		for i, node := range reached {
			work[i] = node.item
		}
		runArtifactInstallWorkers(ctx, work, g.ops)
		for i, node := range reached {
			node.item = work[i]
		}
		level = sortGraphLevel(g.settle(reached))
	}
	g.check()
}

// sortGraphLevel orders a level: the members under an explicit channel
// first, then by name.
func sortGraphLevel(members []graphMember) []graphMember {
	sorted := slices.Clone(members)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if (a.channel != "") != (b.channel != "") {
			return a.channel != ""
		}
		return a.name < b.name
	})
	return sorted
}

// reach records the edges of level and returns the dependencies it reaches
// first, in order.
func (g *installGraph) reach(level []graphMember) []*graphNode {
	var reached []*graphNode
	for _, member := range level {
		for _, name := range sortedStringKeys(member.deps) {
			constraint := member.deps[name]
			g.edges = append(g.edges, graphEdge{from: member.name, to: name, constraint: constraint})
			if g.declared[name] {
				continue
			}
			if node := g.nodes[name]; node != nil {
				if !slices.Contains(node.requiredBy, member.name) {
					node.requiredBy = append(node.requiredBy, member.name)
				}
				continue
			}
			node := &graphNode{name: name, requiredBy: []string{member.name}, fresh: member.fresh}
			if member.channel != "" {
				node.request, node.channel = member.channel, member.channel
			} else {
				node.request = dependencyRequest(constraint)
			}
			g.nodes[name] = node
			g.order = append(g.order, node)
			reached = append(reached, node)
		}
	}
	return reached
}

// dependencyRequest is what a dependency resolves on outside an explicit
// channel: its constraint as written, and latest when it states none.
func dependencyRequest(constraint string) string {
	switch strings.TrimSpace(constraint) {
	case "", "*", "latest":
		return "latest"
	}
	return strings.TrimSpace(constraint)
}

// prepare decides how each node resolves: from its lock entry while that
// satisfies every constraint known now, and otherwise again from the
// registry, a move. A run that writes no lock refuses a node the lock does not
// pin, and a move.
func (g *installGraph) prepare(reached []*graphNode) {
	for _, node := range reached {
		node.known = g.edgesInto(node.name)
		item := artifactInstallWork{name: node.name, constraint: node.request, requiredBy: node.requiredBy[0]}
		item.prior, item.hadPrior = g.ops.lockGet(g.lockFile, node.name)
		switch {
		case !item.hadPrior && !g.persist:
			item.err = g.notPinned(node)
		case item.hadPrior && !node.fresh && satisfiesRanges(item.prior.Version, node.known):
			locked := item.prior
			item.lockEntry = &locked
		case item.hadPrior && !node.fresh && !g.persist:
			item.err = g.lockMisses(node, item.prior.Version)
		}
		node.item = item
	}
}

// settle checks each resolved node against the constraints known when it
// resolved and returns the next level's members.
func (g *installGraph) settle(reached []*graphNode) []graphMember {
	var next []graphMember
	for _, node := range reached {
		item := &node.item
		if item.err != nil {
			continue
		}
		manifest := item.result.Manifest
		switch {
		case manifest != nil && manifest.Name != "" && manifest.Name != node.name:
			item.err = fmt.Errorf("%s requires %s, and the registry build names itself %s", node.requiredBy[0], node.name, manifest.Name)
			continue
		case manifest != nil && manifest.FloorErr != nil:
			item.err = manifest.FloorErr
			continue
		}
		if err := g.checkNode(node, node.known); err != nil {
			item.err = err
			continue
		}
		member := graphMember{name: node.name, channel: node.channel, fresh: node.fresh}
		if manifest != nil {
			member.deps = manifest.Dependencies
		}
		next = append(next, member)
	}
	return next
}

// check checks every constraint of the graph once every node resolved: a
// constraint found after its target resolved, and every constraint on a root
// the workspace declares by name.
func (g *installGraph) check() {
	targets := map[string]bool{}
	for _, edge := range g.edges {
		targets[edge.to] = true
	}
	for _, name := range sortedBoolKeys(targets) {
		edges := g.edgesInto(name)
		if node := g.nodes[name]; node != nil {
			if node.item.err == nil && node.item.result != nil {
				node.item.err = g.checkNode(node, edges)
			}
			continue
		}
		version, ok := g.versions[name]
		if !ok || version == "" || satisfiesRanges(version, edges) {
			continue
		}
		g.rootErrs[name] = unmatchedRange(name, version, edges)
	}
}

// checkNode checks the resolved node against edges. A registry answer to a
// channel that does not name the version it resolved cannot be checked
// against a range, and fails.
func (g *installGraph) checkNode(node *graphNode, edges []graphEdge) error {
	ranges := rangeEdges(edges)
	if len(ranges) == 0 {
		return nil
	}
	result := node.item.result
	if node.item.lockEntry == nil && result.VersionUnnamed && extension.IsChannelRequest(node.request) {
		return fmt.Errorf("%s: the registry answered channel %s without naming the version it resolved, so it cannot be checked against %s",
			node.name, node.request, describeDependents(edges))
	}
	if satisfiesRanges(result.Version, edges) {
		return nil
	}
	return unmatchedRange(node.name, result.Version, edges)
}

func (g *installGraph) edgesInto(name string) []graphEdge {
	var edges []graphEdge
	for _, edge := range g.edges {
		if edge.to == name {
			edges = append(edges, edge)
		}
	}
	return edges
}

// notPinned is the refusal of a dependency the lock does not pin, by a run
// that writes no lock.
func (g *installGraph) notPinned(node *graphNode) error {
	if runcredential.Hosted() {
		return fmt.Errorf("%s: %s requires %s, which the committed %s does not pin; a hosted run writes no lock, so run `putnami install` without %s and commit the lock",
			runcredential.Flag, node.requiredBy[0], node.name, lockfile.LockFilename, runcredential.Flag)
	}
	return fmt.Errorf("%s requires %s, which %s does not pin; run `putnami install`", node.requiredBy[0], node.name, lockfile.LockFilename)
}

// lockMisses is the refusal of a locked dependency that misses a constraint,
// by a run that writes no lock.
func (g *installGraph) lockMisses(node *graphNode, version string) error {
	err := unmatchedRange(node.name, version, node.known)
	if runcredential.Hosted() {
		return fmt.Errorf("%s: %w; a hosted run writes no lock, so run `putnami install` without %s and commit the lock",
			runcredential.Flag, err, runcredential.Flag)
	}
	return fmt.Errorf("%w; run `putnami install` to move it", err)
}

// rangeEdges returns the edges whose constraint is a semantic version range.
func rangeEdges(edges []graphEdge) []graphEdge {
	var ranges []graphEdge
	for _, edge := range edges {
		if extension.IsRangeConstraint(edge.constraint) {
			ranges = append(ranges, edge)
		}
	}
	return ranges
}

// satisfiesRanges reports whether version satisfies every range of edges.
func satisfiesRanges(version string, edges []graphEdge) bool {
	for _, edge := range rangeEdges(edges) {
		if !extension.SatisfiesRange(version, edge.constraint) {
			return false
		}
	}
	return true
}

// unmatchedRange names the dependency, its version, and each dependent with
// its constraint.
func unmatchedRange(name, version string, edges []graphEdge) error {
	return fmt.Errorf("%s@%s does not satisfy %s", name, version, describeDependents(edges))
}

func describeDependents(edges []graphEdge) string {
	parts := make([]string, 0, len(edges))
	for _, edge := range edges {
		constraint := edge.constraint
		if strings.TrimSpace(constraint) == "" {
			constraint = "any version"
		}
		parts = append(parts, fmt.Sprintf("%s, which requires %s", edge.from, constraint))
	}
	return strings.Join(parts, "; ")
}

func sortedBoolKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// failed reports whether a node or a root failed a check of the graph.
func (g *installGraph) failed() bool {
	if len(g.rootErrs) > 0 {
		return true
	}
	for _, node := range g.order {
		if node.item.err != nil {
			return true
		}
	}
	return false
}

// work returns the dependencies' work items in resolution order.
func (g *installGraph) work() []artifactInstallWork {
	work := make([]artifactInstallWork, len(g.order))
	for i, node := range g.order {
		work[i] = node.item
	}
	return work
}

// graphRoots returns the members of level 0: each root in work whose
// manifest names a dependency, and each other root the workspace declares,
// read from disk, when only one extension installs. It records the names the
// workspace satisfies and the versions of the roots it declares by name. A
// root whose manifest claims the dependency rung below
// ExtensionDependenciesContract fails.
//
// channels maps a root to the explicit channel its subgraph resolves on, and
// re-resolves. workFresh makes the dependencies of the roots in work resolve
// again whatever the lock holds, and contextFresh those of the other roots.
func (g *installGraph) graphRoots(cfg *wsproto.Config, work []artifactInstallWork, globalOnly map[string]bool, specific string,
	channels map[string]string, workFresh, contextFresh bool) []graphMember {
	var members []graphMember
	keys := workspaceKeys(cfg, globalOnly)
	for _, key := range keys {
		g.declared[key] = true
	}
	if specific != "" {
		g.declared[specific] = true
	}
	installed := map[string]bool{}
	for i := range work {
		item := &work[i]
		installed[item.name] = true
		if item.err != nil {
			continue
		}
		if item.local {
			root, ok := g.ops.graph.root(item.name, g.lockFile)
			if !ok {
				continue
			}
			g.declared[root.Name] = true
			if root.FloorErr != nil {
				item.err = root.FloorErr
				continue
			}
			if root.Followed {
				members = append(members, graphMember{name: root.Name, deps: root.Dependencies, fresh: workFresh})
			}
			continue
		}
		g.versions[item.name] = item.result.Version
		manifest := item.result.Manifest
		if manifest == nil {
			continue
		}
		if manifest.FloorErr != nil {
			item.err = manifest.FloorErr
			continue
		}
		member := graphMember{name: item.name, deps: manifest.Dependencies, fresh: workFresh}
		if channel := channels[item.name]; channel != "" {
			member.channel, member.fresh = channel, true
		}
		members = append(members, member)
	}
	for _, key := range keys {
		if installed[key] {
			continue
		}
		root, ok := g.ops.graph.root(key, g.lockFile)
		if !ok {
			continue
		}
		name := key
		if root.ByPath {
			name = root.Name
			g.declared[name] = true
		} else if entry, pinned := g.ops.lockGet(g.lockFile, key); pinned {
			g.versions[key] = entry.Version
		}
		if root.Followed && root.FloorErr == nil {
			members = append(members, graphMember{name: name, deps: root.Dependencies, fresh: contextFresh})
		}
	}
	return members
}

// workspaceKeys returns the `extensions` keys of cfg that are not only in the
// global configuration, sorted.
func workspaceKeys(cfg *wsproto.Config, globalOnly map[string]bool) []string {
	if cfg == nil {
		return nil
	}
	var keys []string
	for _, key := range cfg.Extensions.Names() {
		if !globalOnly[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

// member reports whether name is a member of the graph: a root or a
// dependency.
func (g *installGraph) member(name string) bool {
	return g.declared[name] || g.nodes[name] != nil
}

// ignoredGlobalKeys returns the keys of deferred that name a graph member,
// sorted, and warns once for each: the global configuration does not satisfy
// a dependency, and the graph resolves it.
func (g *installGraph) ignoredGlobalKeys(deferred map[string]string, quiet bool) map[string]bool {
	ignored := map[string]bool{}
	for _, name := range sortedStringKeys(deferred) {
		if !g.member(name) {
			continue
		}
		ignored[name] = true
		if !quiet {
			iox.Fprintf(os.Stderr, "  ! %s declares %s, which the workspace's extension graph requires; the graph resolves it, and that key is ignored\n",
				extension.GlobalConfigDisplay, name)
		}
	}
	return ignored
}

// splitGlobalOnly returns the entries of artMap the workspace file declares,
// with specific, and apart the entries only the global configuration declares.
func splitGlobalOnly(artMap map[string]string, globalOnly map[string]bool, specific string) (map[string]string, map[string]string) {
	roots, deferred := map[string]string{}, map[string]string{}
	for name, constraint := range artMap {
		if globalOnly[name] && name != specific {
			deferred[name] = constraint
			continue
		}
		roots[name] = constraint
	}
	return roots, deferred
}

// installGraphWork installs the dependency graph of the roots work installed,
// then the global-only entries of deferred that name no graph member, and
// returns every work item, sorted by name, with whether the graph failed.
func installGraphWork(ctx context.Context, cfg *wsproto.Config, lockFile *lockfile.LockFile, work []artifactInstallWork, deferred map[string]string,
	specificName, specificVersion string, latest, persist bool, ops artifactOps, humanOut io.Writer, globalOnly map[string]bool, quiet bool) ([]artifactInstallWork, bool) {
	graph := newInstallGraph(ops, lockFile, persist)
	channels := map[string]string{}
	if extension.IsExplicitChannel(specificVersion) {
		channels[specificName] = specificVersion
	}
	roots := graph.graphRoots(cfg, work, globalOnly, specificName, channels, latest, latest)
	graph.resolve(ctx, roots)
	failed := graph.failed()
	for i := range work {
		if err, ok := graph.rootErrs[work[i].name]; ok && work[i].err == nil {
			work[i].err = err
			delete(graph.rootErrs, work[i].name)
		}
	}
	for name, err := range graph.rootErrs {
		work = append(work, artifactInstallWork{name: name, err: err})
	}
	work = append(work, graph.work()...)

	ignored := graph.ignoredGlobalKeys(deferred, quiet)
	rest := map[string]string{}
	for name, constraint := range deferred {
		if !ignored[name] {
			rest[name] = constraint
		}
	}
	if len(rest) > 0 {
		later := prepareArtifactInstallWork(lockFile, rest, "", "", latest, ops, humanOut)
		runArtifactInstallWorkers(ctx, later, ops)
		work = append(work, later...)
	}
	sort.SliceStable(work, func(i, j int) bool { return work[i].name < work[j].name })
	return work, failed
}

// errGraphUnresolved ends an install or an update whose graph did not resolve.
var errGraphUnresolved = errors.New("the extension graph did not resolve")

// graphFailure is the error of a run whose graph did not resolve, naming the
// first failure of work: the run wrote no lock.
func graphFailure(work []artifactInstallWork) error {
	for _, item := range work {
		if item.err != nil {
			return fmt.Errorf("%w: %s: %w; %s is unchanged", errGraphUnresolved, item.name, item.err, lockfile.LockFilename)
		}
	}
	return fmt.Errorf("%w; %s is unchanged", errGraphUnresolved, lockfile.LockFilename)
}

// requiredBySuffix renders the dependent of a dependency in a status line.
func requiredBySuffix(requiredBy string, extra ...string) string {
	parts := slices.Clone(extra)
	if requiredBy != "" {
		parts = append(parts, "required by "+requiredBy)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// updateDependencies re-resolves the dependencies of the roots an update
// installed, under the update's explicit channel when it names one, writes
// their lock entries into lockFile and reports each one. It never touches the
// workspace file. A dependency that does not resolve fails the update before
// the lock is written. It returns the graph and how many dependencies moved.
func updateDependencies(ctx context.Context, cfg *wsproto.Config, lockFile *lockfile.LockFile, roots []artifactInstallWork, globalOnly map[string]bool,
	opts ArtifactUpdateOptions, ops artifactOps, quiet bool) (*installGraph, int, error) {
	graph := newInstallGraph(ops, lockFile, true)
	channels := map[string]string{}
	if extension.IsExplicitChannel(opts.ConstraintOverride) {
		for _, root := range roots {
			if !root.local {
				channels[root.name] = opts.ConstraintOverride
			}
		}
	}
	members := graph.graphRoots(cfg, roots, globalOnly, "", channels, true, false)
	graph.resolve(ctx, members)

	failures := slices.Clone(roots)
	for name, err := range graph.rootErrs {
		failures = append(failures, artifactInstallWork{name: name, err: err})
	}
	failures = append(failures, graph.work()...)
	failed := false
	for _, item := range failures {
		if item.err == nil {
			continue
		}
		failed = true
		if quiet {
			emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: item.name, Status: "failed", Error: item.err.Error(), RequiredBy: item.requiredBy})
		} else {
			iox.Fprintf(os.Stderr, "  ✗ %s: %v\n", item.name, item.err)
		}
	}
	if failed {
		return graph, 0, graphFailure(failures)
	}

	moved := 0
	for _, node := range graph.order {
		item := node.item
		entry := lockEntryFor(item.prior, item.hadPrior, item.result)
		carryPlatformIntegrities(ctx, ops, item.name, item.prior, item.hadPrior, &entry, quiet)
		ops.lockSet(lockFile, item.name, entry)
		oldVersion := item.prior.Version
		if oldVersion != "" && oldVersion != item.result.Version {
			moved++
			if quiet {
				emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: item.name, Version: item.result.Version, OldVersion: oldVersion,
					Source: item.result.Source, Status: "updated", RequiredBy: item.requiredBy})
			} else {
				iox.Fprintf(os.Stdout, "  ↑ %s: %s → %s%s\n", item.name, oldVersion, item.result.Version, requiredBySuffix(item.requiredBy))
			}
			continue
		}
		if quiet {
			emitArtifactActionJSONL(artifactActionEntry{Kind: ops.label, Action: "update", Name: item.name, Version: item.result.Version, OldVersion: oldVersion,
				Source: item.result.Source, FromCache: item.result.FromCache, Status: "up-to-date", RequiredBy: item.requiredBy})
		} else {
			iox.Fprintf(os.Stdout, "  ✓ %s@%s%s\n", item.name, item.result.Version, requiredBySuffix(item.requiredBy, "up to date"))
		}
	}
	return graph, moved, nil
}

// previewDependencyNote says that a dry run previewed the declared extensions
// only, when the workspace graph holds a dependency.
func previewDependencyNote(wsRoot string, cfg *wsproto.Config, ops artifactOps, jsonlOut bool) {
	if ops.graph == nil {
		return
	}
	lf, _ := lockfile.ReadLockFile(wsRoot)
	walk := ops.graph.walk(cfg, lf, extension.WalkOptions{})
	if len(walk.Dependencies) == 0 {
		return
	}
	names := make([]string, len(walk.Dependencies))
	for i, dep := range walk.Dependencies {
		names[i] = dep.Name
	}
	out := io.Writer(os.Stdout)
	if jsonlOut {
		out = os.Stderr
	}
	iox.Fprintf(out, "  This preview covers the declared extensions only; `putnami extensions update` also resolves their dependencies again: %s\n",
		strings.Join(names, ", "))
}

// removeExtension removes the declared extension args[0]: its lock entry and
// stable link, and those of each dependency that the remaining declarations
// no longer reach. The remaining declarations are the workspace's without
// args[0], so the root of a cycle can be removed. It refuses an extension the
// remaining declarations still reach, and names the members on that path. It
// leaves the workspace file to the user.
func removeExtension(wsRoot string, cfg *wsproto.Config, args []string, ops artifactOps) error {
	if len(args) == 0 || ops.graph == nil {
		return removeArtifact(wsRoot, args, ops)
	}
	name := args[0]
	lockFile, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return fmt.Errorf("read lock file: %w", err)
	}
	after := ops.graph.walk(cfg, lockFile, extension.WalkOptions{Without: name})
	if _, required := after.Dependency(name); required {
		return fmt.Errorf("extension %s is still required: %s; remove the extension that requires it from %s first",
			name, strings.Join(extension.DependencyPath(after.Dependencies, name), " → "), wsproto.WorkspaceConfigFilename)
	}
	var pruned []string
	before := ops.graph.walk(cfg, lockFile, extension.WalkOptions{Extra: name})
	for _, dep := range before.Dependencies {
		if dep.Name == name || after.Declared[dep.Name] {
			continue
		}
		if _, kept := after.Dependency(dep.Name); !kept {
			pruned = append(pruned, dep.Name)
		}
	}
	sort.Strings(pruned)

	if lockFile != nil {
		ops.lockRemove(lockFile, name)
		for _, dep := range pruned {
			ops.lockRemove(lockFile, dep)
		}
		if err := lockfile.WriteLockFile(wsRoot, lockFile); err != nil {
			return fmt.Errorf("write lock file: %w", err)
		}
	}
	for _, removed := range append([]string{name}, pruned...) {
		if err := ops.remove(removed); err != nil {
			return fmt.Errorf("remove %s: %w", ops.label, err)
		}
	}

	iox.Fprintf(os.Stdout, "  ✓ Removed %s\n", name)
	for _, dep := range pruned {
		iox.Fprintf(os.Stdout, "  ✓ Removed %s, which no declared extension requires any more\n", dep)
	}
	iox.Fprintf(os.Stdout, "  Note: update putnami.workspace.json %s field manually\n", ops.plural)
	return nil
}
