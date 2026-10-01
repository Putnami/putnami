package wsview

import "strings"

// WarningCode is a stable machine-readable reason attached to a workspace
// warning whose meaning must be consumed without matching human prose.
//
// Copied from `go.putnami.dev/cli/model/workspace`. The architecture engine
// branches on the one code below, so the value must stay byte-identical to
// core's: the two sides classify the same workspace.
type WarningCode string

// WarningCodeProviderViewUnavailable means provider-derived project identity or
// dependency edges could not be adopted and are therefore incomplete.
const WarningCodeProviderViewUnavailable WarningCode = "workspace.provider_view_unavailable"

// DependencyGraph is the resolved direct-dependency index over one membership.
//
// Copied from `go.putnami.dev/cli/model/workspace`.DependencyGraph, narrowed to
// the one query the SDD engines make — DependenciesOf, which the architecture
// detector uses to extract cross-domain edges. Cycle detection, topological
// order, path search and the transitive closures were left behind: they belong
// to the scheduler, not to a validator.
type DependencyGraph struct {
	// deps maps a project ID to the IDs of the projects it depends on.
	deps map[string][]string
	// nodes is every project ID in the membership.
	nodes map[string]bool
}

// BuildGraph resolves declared dependencies into ID edges.
//
// Graph keys are project IDs ("/typescript/framework/runtime"). A dependency
// declared as a NAME ("@putnami/runtime") is translated through a name→ID
// lookup; one already in "/path" form is kept; one naming no member is dropped,
// because an out-of-workspace edge has no node to point at.
//
// For an activated scope, an implicit edge is added from each direct include to
// the scope-self project. Copied from core's BuildGraph so a view assembled
// here and a workspace loaded there answer DependenciesOf the same way.
func BuildGraph(projects []*Project) *DependencyGraph {
	nameToID := make(map[string]string, len(projects))
	for _, project := range projects {
		if project == nil {
			continue
		}
		nameToID[project.Name] = project.ID
	}

	graph := &DependencyGraph{
		deps:  make(map[string][]string, len(projects)),
		nodes: make(map[string]bool, len(projects)),
	}
	for _, project := range projects {
		if project == nil {
			continue
		}
		graph.nodes[project.ID] = true
		var deps []string
		for _, dependency := range project.Dependencies {
			if strings.HasPrefix(dependency, "/") {
				deps = append(deps, dependency)
			} else if id, known := nameToID[dependency]; known {
				deps = append(deps, id)
			}
		}
		graph.deps[project.ID] = deps
	}

	for _, project := range projects {
		if project == nil || !project.ActivatedScope {
			continue
		}
		for _, childID := range project.ScopeIncludes {
			if !graph.nodes[childID] || containsString(graph.deps[childID], project.ID) {
				continue
			}
			graph.deps[childID] = append(graph.deps[childID], project.ID)
		}
	}
	return graph
}

// DependenciesOf returns the direct dependencies of a project, by ID.
func (graph *DependencyGraph) DependenciesOf(id string) []string {
	if graph == nil {
		return nil
	}
	return graph.deps[id]
}

func containsString(values []string, item string) bool {
	for _, value := range values {
		if value == item {
			return true
		}
	}
	return false
}
