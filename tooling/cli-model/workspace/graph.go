package workspace

import (
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// DependencyGraph represents the project dependency graph.
type DependencyGraph struct {
	// Adjacency list: project name → names of projects it depends on.
	deps map[string][]string
	// Reverse adjacency: project name → names of projects that depend on it.
	dependents map[string][]string
	// impactDependents is dependents minus the implicit include→scope-self
	// edges recorded in scopeEdges. Those edges order the schedule
	// (workspace-level artifacts before their workloads) and carry no input
	// relation: a scope-self's own files are read by none of its includes, so a
	// change to them reaches an include only through a declared dependency.
	// --impacted and why_impacted walk this family.
	impactDependents map[string][]string
	// All project names
	nodes map[string]bool
	// scopeEdges records the IMPLICIT edges BuildGraph adds for activated
	// scopes: scope ID → child IDs that did not declare the scope themselves.
	// They sit in deps and dependents like any declared edge, so this is the
	// only place that can tell the two apart — impact tracing needs it to explain an
	// edge, and impact propagation needs it to keep the edge out of impactDependents.
	scopeEdges map[string]map[string]bool
	// contractClients maps a provider's project ID to the IDs of the generated
	// client targets derived from its contract, in project-input order.
	//
	// A contract edge is its own family. It is NOT in deps or dependents: those
	// are the functional edges a dependent's actions read, and a generated
	// client reads its contract, never its provider's sources. It is not in
	// impactDependents either — propagateImpact walks it separately so the
	// trace can name it as the reason a client was selected.
	contractClients map[string][]string
	// contractProvider is the inverse of contractClients: a generated client's
	// project ID → the provider whose contract it was generated from. One
	// client has at most one provider: its manifest names exactly one service.
	contractProvider map[string]string
	// edgeSources records each edge's PROVENANCE: dependent ID → dependency ID
	// → the manifest family the edge was derived from. Every edge of deps and
	// of the contract family has an entry, so EdgeSource is total over the
	// graph's edges and an absent entry means "no edge", never "unknown
	// origin".
	//
	// Provenance does not change the edge SET. It answers a question the set
	// cannot: whether the build really reads that project, or whether a
	// declaration merely says so.
	edgeSources map[string]map[string]wsproto.DependencySource
}

// BuildGraph constructs a dependency graph from discovered projects.
// Graph keys are project IDs (e.g. "/typescript/framework/runtime").
// Dependencies declared as Names (e.g. "@putnami/runtime") are translated
// to IDs via a Name→ID lookup; dependencies already in /path form are kept.
//
// For activated scopes, an implicit edge is added from each direct include
// to the scope-self project (child depends on scope). This ensures
// workspace-level artifacts run before the workloads that consume them.
// Use FindCycle to surface cycles introduced by these implicit edges. The
// implicit edge is an ORDERING edge only: it is left out of impactDependents,
// so a change to the scope-self's own files does not select every include.
func BuildGraph(projects []*Project) *DependencyGraph {
	// Build Name→ID lookup for translating Name-based dependencies
	// (e.g. "@putnami/runtime") to project IDs.
	nameToID := make(map[string]string, len(projects))
	for _, p := range projects {
		nameToID[p.Name] = p.ID
	}

	g := &DependencyGraph{
		deps:             make(map[string][]string),
		dependents:       make(map[string][]string),
		impactDependents: make(map[string][]string),
		nodes:            make(map[string]bool),
		scopeEdges:       make(map[string]map[string]bool),
		contractClients:  make(map[string][]string),
		contractProvider: make(map[string]string),
		edgeSources:      make(map[string]map[string]wsproto.DependencySource),
	}

	// Resolve declared dependencies first.
	for _, p := range projects {
		g.nodes[p.ID] = true

		var deps []string
		for _, dep := range p.Dependencies {
			id := ""
			if strings.HasPrefix(dep, "/") {
				id = dep // Already an ID
			} else if translated, ok := nameToID[dep]; ok {
				id = translated // Translate Name → ID
			}
			// External deps (not in workspace) are silently ignored.
			if id == "" {
				continue
			}
			deps = append(deps, id)
			// The provenance travels with the edge under the SAME spelling the
			// dependency was declared with, because that is the key the
			// provider answered under and the one the project carries.
			g.recordEdgeSource(p.ID, id, dependencySourceOf(p, dep))
		}
		g.deps[p.ID] = deps
	}

	// Layer in implicit edges from activated scopes' includes back to the scope-self.
	// Skip when a child already declares the scope as a dependency (idempotent):
	// a declared edge is a full edge and carries impact.
	//
	// scopeEdges records the edges layered in here, which is what lets the
	// reverse index below leave them out of the impact family while keeping
	// them in the ordering family.
	for _, p := range projects {
		if !p.ActivatedScope {
			continue
		}
		for _, childID := range p.ScopeIncludes {
			if !g.nodes[childID] {
				continue
			}
			if containsString(g.deps[childID], p.ID) {
				continue
			}
			g.deps[childID] = append(g.deps[childID], p.ID)
			if g.scopeEdges[p.ID] == nil {
				g.scopeEdges[p.ID] = make(map[string]bool)
			}
			g.scopeEdges[p.ID][childID] = true
			// An implicit scope edge orders the schedule and reads nothing, so
			// its provenance is a declaration — the scope's own membership
			// list — rather than an import.
			g.recordEdgeSource(childID, p.ID, wsproto.DependencySourceDeclared)
		}
	}

	// Build reverse adjacency in project-input order so DependentsOf returns
	// a stable result. Map iteration would shuffle these and pollute display
	// output (projects info, --plan, etc.).
	for _, p := range projects {
		for _, dep := range g.deps[p.ID] {
			g.dependents[dep] = append(g.dependents[dep], p.ID)
			if !g.scopeEdges[dep][p.ID] {
				g.impactDependents[dep] = append(g.impactDependents[dep], p.ID)
			}
		}
	}

	g.deriveContractEdges(projects)
	return g
}

// deriveContractEdges resolves each generated client target onto the provider
// whose committed contract declares the service its manifest names.
//
// The rule is the manifest's own words and nothing else: `service.id` in a
// project's committed client.putnami.json names the provider, and the provider
// is the project whose committed contract declares that same identity. Nearness
// in the tree is deliberately not a rule — a client that lives beside its
// provider and one that lives in another scope are the same relation, and the
// cloud workspaces this serves keep clients where their consumers are.
//
// A service identity two projects both declare resolves to NEITHER: the edge
// would then depend on project order, and an ambiguous provider is not a
// provider. A self-edge is dropped for the same reason a project never depends
// on itself.
func (g *DependencyGraph) deriveContractEdges(projects []*Project) {
	providerOf := make(map[string]string, len(projects))
	ambiguous := make(map[string]bool)
	for _, p := range projects {
		if p.ContractServiceID == "" {
			continue
		}
		if _, taken := providerOf[p.ContractServiceID]; taken {
			ambiguous[p.ContractServiceID] = true
			continue
		}
		providerOf[p.ContractServiceID] = p.ID
	}

	for _, p := range projects {
		if p.GeneratedClient == nil {
			continue
		}
		service := p.GeneratedClient.ServiceID
		if ambiguous[service] {
			continue
		}
		providerID, ok := providerOf[service]
		if !ok || providerID == p.ID || !g.nodes[providerID] {
			continue
		}
		if containsString(g.contractClients[providerID], p.ID) {
			continue
		}
		g.contractClients[providerID] = append(g.contractClients[providerID], p.ID)
		g.contractProvider[p.ID] = providerID
		g.recordEdgeSource(p.ID, providerID, wsproto.DependencySourceContract)
	}
}

// recordEdgeSource attributes one edge. An edge described twice keeps the
// stronger attribution, which is how a project that both declares a dependency
// and imports it reads as an import.
func (g *DependencyGraph) recordEdgeSource(dependent, dependency string, source wsproto.DependencySource) {
	if g.edgeSources[dependent] == nil {
		g.edgeSources[dependent] = make(map[string]wsproto.DependencySource, 4)
	}
	g.edgeSources[dependent][dependency] = wsproto.StrongerDependencySource(
		g.edgeSources[dependent][dependency], source)
}

// dependencySourceOf answers where one of a project's declared dependency
// entries came from. An entry no provider attributed is a declaration: the
// authored putnami.json (or the artifact base a config names) states it, and
// nothing was read from an import to produce it.
func dependencySourceOf(p *Project, dependency string) wsproto.DependencySource {
	if source, ok := p.DependencySources[dependency]; ok && source.Valid() {
		return source
	}
	return wsproto.DependencySourceDeclared
}

// EdgeSource is the provenance of one edge: the manifest family it was derived
// from, or "" when the graph carries no edge from dependent to dependency.
//
// It covers the contract family too, which is not in deps: a generated client
// answers DependencySourceContract for its provider.
func (g *DependencyGraph) EdgeSource(dependent, dependency string) wsproto.DependencySource {
	if g == nil {
		return ""
	}
	return g.edgeSources[dependent][dependency]
}

// ContractClientsOf returns the generated client targets derived from a
// project's contract, in project-input order. Empty for a project that provides
// none.
func (g *DependencyGraph) ContractClientsOf(providerID string) []string {
	if g == nil {
		return nil
	}
	return g.contractClients[providerID]
}

// ContractProviderOf returns the project whose contract a generated client was
// generated from, or "" when this project is not a generated client or its
// provider is not in the workspace.
func (g *DependencyGraph) ContractProviderOf(clientID string) string {
	if g == nil {
		return ""
	}
	return g.contractProvider[clientID]
}

// FindCycle inspects the dependency graph and returns the participants of any
// cycle as a slice that starts and ends at the same node. Returns nil when
// the graph is acyclic.
func (g *DependencyGraph) FindCycle() []string {
	if g == nil {
		return nil
	}
	return findCycle(g.nodes, g.deps)
}

// findCycle returns a slice naming the nodes participating in a cycle, or nil
// when the graph is acyclic. The returned slice starts and ends at the same
// node so callers can render it as "A → B → C → A".
func findCycle(nodes map[string]bool, deps map[string][]string) []string {
	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make(map[string]int, len(nodes))
	parent := make(map[string]string, len(nodes))

	// Sort node IDs for deterministic cycle reporting.
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var cycleEnd, cycleStart string
	var found bool

	var dfs func(id string) bool
	dfs = func(id string) bool {
		state[id] = onStack
		neighbors := append([]string(nil), deps[id]...)
		sort.Strings(neighbors)
		for _, next := range neighbors {
			if !nodes[next] {
				continue
			}
			switch state[next] {
			case unvisited:
				parent[next] = id
				if dfs(next) {
					return true
				}
			case onStack:
				cycleEnd = id
				cycleStart = next
				found = true
				return true
			}
		}
		state[id] = done
		return false
	}

	for _, id := range ids {
		if state[id] == unvisited {
			if dfs(id); found {
				break
			}
		}
	}

	if !found {
		return nil
	}

	// Walk parents from cycleEnd back to cycleStart to recover the cycle.
	var path []string
	for n := cycleEnd; n != cycleStart; n = parent[n] {
		path = append(path, n)
		if _, ok := parent[n]; !ok {
			break
		}
	}
	path = append(path, cycleStart)
	// Reverse to get root → ... → root order.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	path = append(path, cycleStart)
	return path
}

func containsString(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// DependenciesOf returns the direct dependencies of a project.
func (g *DependencyGraph) DependenciesOf(name string) []string {
	return g.deps[name]
}

// DependentsOf returns the projects that directly depend on the given project.
// It answers the ORDERING family, implicit include→scope-self edges included;
// impact reads ImpactDependentsOf.
func (g *DependencyGraph) DependentsOf(name string) []string {
	return g.dependents[name]
}

// ImplicitScopeEdge reports whether the edge from scopeID to childID is one
// BuildGraph added on its own for an activated scope, as opposed to a
// dependency the child declared. A child that declares its scope keeps a
// declared edge and answers false here.
func (g *DependencyGraph) ImplicitScopeEdge(scopeID, childID string) bool {
	if g == nil {
		return false
	}
	return g.scopeEdges[scopeID][childID]
}

// ImpactDependentsOf returns the projects a change to name reaches through one
// declared or provider-derived edge: DependentsOf without the edges
// ImplicitScopeEdge reports, which order the schedule and carry no inputs.
func (g *DependencyGraph) ImpactDependentsOf(name string) []string {
	return g.impactDependents[name]
}

// TopologicalOrder returns projects in dependency-first order: every project
// appears after the workspace projects it depends on. The boolean is false if a
// cycle prevents a complete order.
func (g *DependencyGraph) TopologicalOrder() ([]string, bool) {
	if g == nil {
		return nil, true
	}
	indegree := make(map[string]int, len(g.nodes))
	for id := range g.nodes {
		indegree[id] = 0
		for _, dep := range g.deps[id] {
			if g.nodes[dep] {
				indegree[id]++
			}
		}
	}
	ready := make([]string, 0, len(g.nodes))
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	var order []string
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		order = append(order, current)
		dependents := append([]string(nil), g.dependents[current]...)
		sort.Strings(dependents)
		for _, dependent := range dependents {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
		sort.Strings(ready)
	}
	return order, len(order) == len(g.nodes)
}

// DependencyPath returns the shortest path from one project to another by
// following dependency edges (project -> projects it depends on). The returned
// slice includes both endpoints. Nil means no path, or either endpoint is
// unknown.
func (g *DependencyGraph) DependencyPath(from, to string) []string {
	if g == nil {
		return nil
	}
	return graphPath(g.nodes, g.deps, from, to)
}

// DependentPath returns the shortest path from one project to another by
// following dependent edges (changed project -> projects impacted by it). The
// returned slice includes both endpoints. Nil means no impact path, or either
// endpoint is unknown. It follows IMPACT edges (ImpactDependentsOf), so its
// answer agrees with --impacted: an implicit include→scope-self edge is never
// reported as a reason for an impact that selection does not perform.
func (g *DependencyGraph) DependentPath(from, to string) []string {
	if g == nil {
		return nil
	}
	return graphPath(g.nodes, g.impactDependents, from, to)
}

func graphPath(nodes map[string]bool, edges map[string][]string, from, to string) []string {
	if !nodes[from] || !nodes[to] {
		return nil
	}
	if from == to {
		return []string{from}
	}

	seen := map[string]bool{from: true}
	parent := make(map[string]string, len(nodes))
	queue := []string{from}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		neighbors := append([]string(nil), edges[current]...)
		sort.Strings(neighbors)
		for _, next := range neighbors {
			if !nodes[next] || seen[next] {
				continue
			}
			seen[next] = true
			parent[next] = current
			if next == to {
				path := []string{to}
				for n := to; n != from; {
					n = parent[n]
					path = append(path, n)
				}
				for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
					path[i], path[j] = path[j], path[i]
				}
				return path
			}
			queue = append(queue, next)
		}
	}
	return nil
}

// TransitiveDependenciesOf returns the forward transitive closure of the
// given projects: the seed projects themselves plus every project
// reachable by following dependency edges. Result is sorted for
// deterministic output. Edges to projects outside the workspace were
// already dropped at BuildGraph time, so the closure stays within known
// nodes.
func (g *DependencyGraph) TransitiveDependenciesOf(names []string) []string {
	visited := make(map[string]bool)
	queue := make([]string, len(names))
	copy(queue, names)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current] {
			continue
		}
		visited[current] = true
		for _, dep := range g.deps[current] {
			if !visited[dep] {
				queue = append(queue, dep)
			}
		}
	}

	result := make([]string, 0, len(visited))
	for name := range visited {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// TransitiveDependentsOf returns the forward closure of the given projects in
// the ORDERING family: every project reachable by following dependent edges,
// implicit include→scope-self edges included. It answers "what runs after
// these", not "what a change to these selects" — impact propagation closes over
// ImpactDependentsOf instead.
func (g *DependencyGraph) TransitiveDependentsOf(names []string) []string {
	visited := make(map[string]bool)
	queue := make([]string, len(names))
	copy(queue, names)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current] {
			continue
		}
		visited[current] = true
		for _, dep := range g.dependents[current] {
			if !visited[dep] {
				queue = append(queue, dep)
			}
		}
	}

	result := make([]string, 0, len(visited))
	for name := range visited {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// DeclaredInputRoots returns the workspace-relative paths a project's declared
// file inputs read from.
//
// It answers "what does this project READ, outside its own directory, because
// it said so?" — the third way an edge can be real, beside an import and a
// contract. A project that declares `../contributor/src/**` as a test input
// reads that project's sources: the build hashes them into its key, a change to
// them selects it, and the edge is not a phantom even though no import crosses.
//
// The patterns are the ones declaredFilePatterns reads, so this is the SAME
// declaration the cache hashes and --impacted seeds from — never a second
// reading of it. What this adds is the direction: the index answers "who reads
// this path", and an edge needs "does this reader read anything under that
// project".
//
// A pattern is reduced to its LITERAL PREFIX: the segments before the first one
// carrying a glob metacharacter, resolved against the declaring project's own
// root. That is the deepest directory the pattern can possibly select from, so
// a caller comparing it against a project root can answer containment without
// enumerating the tree — and the answer never depends on what happens to exist
// on disk, which a glob expansion would.
//
// An EXCLUSION (`!…`) is not a read and contributes nothing. A pattern that
// resolves outside the workspace root reads no project of it and is dropped.
// A pattern that resolves TO the workspace root is kept as "." — it reads
// everywhere, which is what its author declared.
func DeclaredInputRoots(p *Project) []string {
	if p == nil {
		return nil
	}
	base := CleanWorkspacePath(p.Path)
	var roots []string
	for _, set := range declaredFilePatterns(p) {
		for _, pattern := range set {
			if strings.HasPrefix(pattern, "!") {
				continue
			}
			if root, ok := resolveDeclaredInputRoot(base, pattern); ok {
				roots = append(roots, root)
			}
		}
	}
	sort.Strings(roots)
	return slices.Compact(roots)
}

// resolveDeclaredInputRoot reduces one authored pattern to the workspace-
// relative path it reads from, and reports whether that path is inside the
// workspace at all.
func resolveDeclaredInputRoot(base, pattern string) (string, bool) {
	cleaned := path.Clean(filepath.ToSlash(strings.TrimSpace(pattern)))
	if cleaned == "" || cleaned == "." {
		return ".", true
	}
	segments := make([]string, 0, 8)
	segments = append(segments, base)
	for _, segment := range strings.Split(cleaned, "/") {
		if strings.ContainsAny(segment, "*?[{") {
			break
		}
		segments = append(segments, segment)
	}
	root := path.Join(segments...)
	if root == ".." || strings.HasPrefix(root, "../") {
		return "", false
	}
	if root == "" {
		root = "."
	}
	return root, true
}

// InputRootReadsProject reports whether a declared input root reads inside a
// project's own directory.
//
// Both containments count. A root INSIDE the project is an obvious read; a root
// that is an ANCESTOR of the project is a pattern whose glob tail can reach into
// it, and counting it is the safe direction: a declaration read too widely
// leaves a stale edge, one read too narrowly deletes a real dependency.
func InputRootReadsProject(root, projectPath string) bool {
	if root == "" {
		return false
	}
	target := CleanWorkspacePath(projectPath)
	if root == "." || target == "" {
		return true
	}
	return root == target || strings.HasPrefix(root, target+"/") || strings.HasPrefix(target, root+"/")
}
