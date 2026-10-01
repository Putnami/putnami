package workspace

import (
	"sort"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestBuildGraph(t *testing.T) {
	projects := []*Project{
		{ID: "/A", Name: "A", Dependencies: []string{"B", "C"}},
		{ID: "/B", Name: "B", Dependencies: []string{"C"}},
		{ID: "/C", Name: "C"},
	}

	g := BuildGraph(projects)

	// Check dependencies
	deps := g.DependenciesOf("/A")
	sort.Strings(deps)
	if len(deps) != 2 || deps[0] != "/B" || deps[1] != "/C" {
		t.Errorf("DependenciesOf(/A) = %v, want [/B, /C]", deps)
	}

	// Check dependents
	dependents := g.DependentsOf("/C")
	sort.Strings(dependents)
	if len(dependents) != 2 || dependents[0] != "/A" || dependents[1] != "/B" {
		t.Errorf("DependentsOf(/C) = %v, want [/A, /B]", dependents)
	}
}

func TestBuildGraph_UsesLogicalIDsForGroupedPaths(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "canonical-identity", "grouped-paths-resolve-to-logical-ids")
	clientID := ProjectIDFromPath("identity/(libs)/identity-client")
	authID := ProjectIDFromPath("identity/(workloads)/auth-server")
	g := BuildGraph([]*Project{
		{ID: clientID, Name: "identity-client", Path: "identity/(libs)/identity-client"},
		{ID: authID, Name: "auth-server", Path: "identity/(workloads)/auth-server", Dependencies: []string{"identity-client"}},
	})

	if got := g.DependenciesOf(authID); len(got) != 1 || got[0] != clientID {
		t.Errorf("DependenciesOf(%q) = %v, want [%s]", authID, got, clientID)
	}
	if got := g.DependenciesOf("/identity/(workloads)/auth-server"); got != nil {
		t.Errorf("physical path-derived ID unexpectedly exists in graph: %v", got)
	}
}

func TestDependencyGraph_TransitiveDependents(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "transitive-closures-are-deterministic")
	projects := []*Project{
		{ID: "/app", Name: "app", Dependencies: []string{"lib-a", "lib-b"}},
		{ID: "/lib-a", Name: "lib-a", Dependencies: []string{"utils"}},
		{ID: "/lib-b", Name: "lib-b", Dependencies: []string{"utils"}},
		{ID: "/utils", Name: "utils"},
	}

	g := BuildGraph(projects)
	affected := g.TransitiveDependentsOf([]string{"/utils"})
	sort.Strings(affected)

	// utils change → lib-a, lib-b, app affected (+ utils itself)
	if len(affected) != 4 {
		t.Fatalf("affected = %v, want [/app, /lib-a, /lib-b, /utils]", affected)
	}
	expected := []string{"/app", "/lib-a", "/lib-b", "/utils"}
	for i, id := range expected {
		if affected[i] != id {
			t.Errorf("affected[%d] = %q, want %q", i, affected[i], id)
		}
	}
}

func TestDependencyGraph_TransitiveDependencies(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "transitive-closures-are-deterministic")
	projects := []*Project{
		{ID: "/app", Name: "app", Dependencies: []string{"lib-a", "lib-b"}},
		{ID: "/lib-a", Name: "lib-a", Dependencies: []string{"utils"}},
		{ID: "/lib-b", Name: "lib-b", Dependencies: []string{"utils"}},
		{ID: "/utils", Name: "utils"},
		{ID: "/unrelated", Name: "unrelated"},
	}

	g := BuildGraph(projects)
	closure := g.TransitiveDependenciesOf([]string{"/app"})

	// app closure → app itself + lib-a, lib-b, utils; never /unrelated.
	expected := []string{"/app", "/lib-a", "/lib-b", "/utils"}
	if len(closure) != len(expected) {
		t.Fatalf("closure = %v, want %v", closure, expected)
	}
	for i, id := range expected {
		if closure[i] != id {
			t.Errorf("closure[%d] = %q, want %q (result must be sorted)", i, closure[i], id)
		}
	}
}

func TestDependencyGraph_TransitiveDependencies_Cycle(t *testing.T) {
	// A self-referential edge must not loop forever.
	projects := []*Project{
		{ID: "/a", Name: "a", Dependencies: []string{"b"}},
		{ID: "/b", Name: "b", Dependencies: []string{"a"}},
	}
	g := BuildGraph(projects)
	closure := g.TransitiveDependenciesOf([]string{"/a"})
	if len(closure) != 2 {
		t.Fatalf("closure = %v, want [/a /b]", closure)
	}
}

func TestDependencyGraph_Paths(t *testing.T) {
	g := BuildGraph([]*Project{
		{ID: "/app", Name: "app", Dependencies: []string{"lib"}},
		{ID: "/lib", Name: "lib", Dependencies: []string{"core"}},
		{ID: "/core", Name: "core"},
		{ID: "/docs", Name: "docs"},
	})

	depPath := g.DependencyPath("/app", "/core")
	if got, want := depPath, []string{"/app", "/lib", "/core"}; len(got) != len(want) {
		t.Fatalf("DependencyPath = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("DependencyPath = %v, want %v", got, want)
			}
		}
	}

	impactPath := g.DependentPath("/core", "/app")
	if got, want := impactPath, []string{"/core", "/lib", "/app"}; len(got) != len(want) {
		t.Fatalf("DependentPath = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("DependentPath = %v, want %v", got, want)
			}
		}
	}

	if path := g.DependentPath("/docs", "/app"); path != nil {
		t.Fatalf("DependentPath unrelated = %v, want nil", path)
	}
	if path := g.DependencyPath("/app", "/app"); len(path) != 1 || path[0] != "/app" {
		t.Fatalf("self DependencyPath = %v, want [/app]", path)
	}
}

func TestDependencyGraph_TopologicalOrder(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "topological-order-is-stable")
	g := BuildGraph([]*Project{
		{ID: "/app", Name: "app", Dependencies: []string{"lib-a", "lib-b"}},
		{ID: "/lib-a", Name: "lib-a", Dependencies: []string{"core"}},
		{ID: "/lib-b", Name: "lib-b", Dependencies: []string{"core"}},
		{ID: "/core", Name: "core"},
	})

	order, ok := g.TopologicalOrder()
	if !ok {
		t.Fatal("TopologicalOrder reported a cycle for an acyclic graph")
	}
	pos := make(map[string]int, len(order))
	for i, id := range order {
		pos[id] = i
	}
	for project, deps := range map[string][]string{
		"/app":   {"/lib-a", "/lib-b"},
		"/lib-a": {"/core"},
		"/lib-b": {"/core"},
	} {
		for _, dep := range deps {
			if pos[dep] >= pos[project] {
				t.Fatalf("order = %v; dependency %s must appear before %s", order, dep, project)
			}
		}
	}
}

func TestDependencyGraph_TopologicalOrderIgnoresExternalPathDeps(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "topological-order-is-stable")
	g := BuildGraph([]*Project{
		{ID: "/app", Name: "app", Dependencies: []string{"/external/project", "lib"}},
		{ID: "/lib", Name: "lib"},
	})

	order, ok := g.TopologicalOrder()
	if !ok {
		t.Fatal("TopologicalOrder reported a cycle for a graph with an external path dependency")
	}
	if len(order) != 2 || order[0] != "/lib" || order[1] != "/app" {
		t.Fatalf("order = %v, want [/lib /app]", order)
	}
}

func TestDependencyGraph_TopologicalOrderCycle(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "a-declared-cycle-is-reported")
	g := BuildGraph([]*Project{
		{ID: "/a", Name: "a", Dependencies: []string{"b"}},
		{ID: "/b", Name: "b", Dependencies: []string{"a"}},
	})
	if _, ok := g.TopologicalOrder(); ok {
		t.Fatal("TopologicalOrder ok = true, want false for a cycle")
	}
}

func TestDependencyGraph_TransitiveDependents_Deterministic(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "transitive-closures-are-deterministic")
	projects := []*Project{
		{ID: "/app", Name: "app", Dependencies: []string{"lib-a", "lib-b"}},
		{ID: "/lib-a", Name: "lib-a", Dependencies: []string{"utils"}},
		{ID: "/lib-b", Name: "lib-b", Dependencies: []string{"utils"}},
		{ID: "/utils", Name: "utils"},
	}

	g := BuildGraph(projects)
	first := g.TransitiveDependentsOf([]string{"/utils"})

	for i := 0; i < 100; i++ {
		result := g.TransitiveDependentsOf([]string{"/utils"})
		for j, id := range result {
			if id != first[j] {
				t.Fatalf("run %d: affected[%d] = %q, want %q", i, j, id, first[j])
			}
		}
	}
}

// TestBuildGraph_ActivatedScopeImplicitEdge verifies that an activated scope
// becomes an implicit upstream dependency of each direct include — the basis
// for ^job resolving to the scope's job — and that the edge stays in the
// ordering family only: it never carries impact from the scope-self to its
// includes.
func TestBuildGraph_ActivatedScopeImplicitEdge(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "implicit-scope-edges-order-without-propagating-impact")
	projects := []*Project{
		{
			ID:             "/cloud",
			Name:           "cloud",
			ActivatedScope: true,
			ScopeIncludes:  []string{"/cloud/workloads/api", "/cloud/workloads/worker"},
		},
		{ID: "/cloud/workloads/api", Name: "api"},
		{ID: "/cloud/workloads/worker", Name: "worker"},
	}

	g := BuildGraph(projects)

	apiDeps := g.DependenciesOf("/cloud/workloads/api")
	if len(apiDeps) != 1 || apiDeps[0] != "/cloud" {
		t.Errorf("api deps = %v; want [/cloud]", apiDeps)
	}

	workerDeps := g.DependenciesOf("/cloud/workloads/worker")
	if len(workerDeps) != 1 || workerDeps[0] != "/cloud" {
		t.Errorf("worker deps = %v; want [/cloud]", workerDeps)
	}

	dependents := g.DependentsOf("/cloud")
	sort.Strings(dependents)
	if len(dependents) != 2 || dependents[0] != "/cloud/workloads/api" || dependents[1] != "/cloud/workloads/worker" {
		t.Errorf("scope dependents = %v; want [api, worker]", dependents)
	}

	// The implicit edge orders the schedule and nothing else: a change to the
	// scope-self reaches no include through it, and why_impacted agrees.
	if impact := g.ImpactDependentsOf("/cloud"); impact != nil {
		t.Errorf("scope impact dependents = %v; want nil (implicit edges carry no impact)", impact)
	}
	if path := g.DependentPath("/cloud", "/cloud/workloads/api"); path != nil {
		t.Errorf("DependentPath(scope, include) = %v; want nil (implicit edges carry no impact)", path)
	}
}

// An implicit scope edge and a declared dependency are indistinguishable in
// deps and dependents — both are how a child gets pulled in when the scope
// changes — so the graph records which ones it added itself. Impact tracing
// reads it to say "your scope changed" rather than "a dependency changed".
func TestBuildGraph_RecordsImplicitScopeEdgesOnly(t *testing.T) {
	projects := []*Project{
		{
			ID:             "/cloud",
			Name:           "cloud",
			ActivatedScope: true,
			ScopeIncludes:  []string{"/cloud/workloads/api", "/cloud/workloads/worker"},
		},
		{ID: "/cloud/workloads/api", Name: "api"},
		// Declares the scope itself, so BuildGraph adds no implicit edge.
		{ID: "/cloud/workloads/worker", Name: "worker", Dependencies: []string{"/cloud"}},
	}

	g := BuildGraph(projects)

	if !g.ImplicitScopeEdge("/cloud", "/cloud/workloads/api") {
		t.Error("the implicit include is not recorded as a scope edge")
	}
	if g.ImplicitScopeEdge("/cloud", "/cloud/workloads/worker") {
		t.Error("a child that declared the scope is recorded as a scope edge, want the declared edge")
	}
	if g.ImplicitScopeEdge("/cloud/workloads/api", "/cloud") {
		t.Error("the scope edge is recorded in the dependent direction, want scope → child")
	}
	var nilGraph *DependencyGraph
	if nilGraph.ImplicitScopeEdge("/cloud", "/cloud/workloads/api") {
		t.Error("a nil graph claims a scope edge")
	}
}

func TestBuildGraph_ActivatedScopeIdempotentWithDeclaredDep(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "implicit-scope-edges-order-without-propagating-impact")
	projects := []*Project{
		{
			ID:             "/cloud",
			Name:           "cloud",
			ActivatedScope: true,
			ScopeIncludes:  []string{"/cloud/api"},
		},
		// api already declares an explicit dep on the scope.
		{ID: "/cloud/api", Name: "api", Dependencies: []string{"/cloud"}},
	}

	g := BuildGraph(projects)

	deps := g.DependenciesOf("/cloud/api")
	if len(deps) != 1 || deps[0] != "/cloud" {
		t.Errorf("api deps = %v; want exactly [/cloud] (no duplicate)", deps)
	}

	// A declared edge is a full edge: the include is selected when the
	// scope-self changes, and why_impacted can explain it.
	if impact := g.ImpactDependentsOf("/cloud"); len(impact) != 1 || impact[0] != "/cloud/api" {
		t.Errorf("scope impact dependents = %v; want [/cloud/api] (declared edge carries impact)", impact)
	}
	if path := g.DependentPath("/cloud", "/cloud/api"); len(path) != 2 || path[0] != "/cloud" || path[1] != "/cloud/api" {
		t.Errorf("DependentPath(scope, declared dependent) = %v; want [/cloud /cloud/api]", path)
	}
}

func TestBuildGraph_FindCycle_NoCycle(t *testing.T) {
	g := BuildGraph([]*Project{
		{ID: "/A", Dependencies: []string{"B"}, Name: "A"},
		{ID: "/B", Name: "B"},
	})
	if cycle := g.FindCycle(); cycle != nil {
		t.Errorf("FindCycle = %v; want nil", cycle)
	}
}

func TestBuildGraph_FindCycle_DeclaredCycle(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "dependency-graph", "a-declared-cycle-is-reported")
	g := BuildGraph([]*Project{
		{ID: "/A", Dependencies: []string{"B"}, Name: "A"},
		{ID: "/B", Dependencies: []string{"A"}, Name: "B"},
	})
	cycle := g.FindCycle()
	if cycle == nil {
		t.Fatal("FindCycle returned nil; expected a cycle")
	}
	if cycle[0] != cycle[len(cycle)-1] {
		t.Errorf("cycle should start and end at the same node: %v", cycle)
	}
}

// TestBuildGraph_FindCycle_ActivatedScopeViaDeclaredDep covers the case the
// user asked us to guard against: a child explicitly depends on an artifact
// that itself transitively depends on the activated scope. The implicit edge
// closes the cycle.
func TestBuildGraph_FindCycle_ActivatedScopeViaDeclaredDep(t *testing.T) {
	projects := []*Project{
		{
			ID:             "/cloud",
			Name:           "cloud",
			ActivatedScope: true,
			ScopeIncludes:  []string{"/cloud/api"},
			// scope-self declares it depends on api → forms a cycle through
			// the implicit api→scope edge.
			Dependencies: []string{"/cloud/api"},
		},
		{ID: "/cloud/api", Name: "api"},
	}
	g := BuildGraph(projects)
	if cycle := g.FindCycle(); cycle == nil {
		t.Fatal("expected cycle through scope-self ↔ api implicit edge")
	}
}
