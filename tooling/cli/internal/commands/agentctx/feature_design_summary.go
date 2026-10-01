package agentctx

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	featureproto "go.putnami.dev/protocol/features"
)

// One project's product-feature summaries, read from its optional build design
// graph for the agent_context payload.
//
// # Why this lives here
//
// It was internal/commands/sdd's until a later change moved the SDD vertical into
// the @putnami/sdd extension. agent_context is NOT part of that vertical — it
// is core's orientation surface — and it was the only caller left, so the code
// followed its caller instead of following its former neighbors. The
// vertical-isolation ratchet (internal/cli/vertical_isolation_ratchet_test.go)
// carried an `agentctx → sdd` exception for exactly these symbols; the
// exception is gone with the edge.
//
// The reduction is deliberate: it reads one project's graph artifact and
// summarizes its feature nodes. It does NOT discover graphs across a workspace,
// merge them with durable manifests, or evaluate evidence. Those are the
// extension's questions, and asking them here is what would recreate the
// coupling the extraction removed.

// FeatureDesignSummary is one semantic feature identity declared by a project's
// design graph.
//
// The JSON is the shape agent_context has always emitted for this field. The
// wider catalog projection carried two more members — the durable manifest
// declarations that minted an identity, and the exact artifacts a divergent
// declaration came from — which this path never populated, because it reads one
// project's single graph and no manifest at all. Both were `omitempty`, so
// leaving them out changes no byte an agent has ever received.
type FeatureDesignSummary struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Outcome  string   `json:"outcome"`
	Owner    string   `json:"owner"`
	Projects []string `json:"projects"`
	// Conflicts names the projects whose declaration of this identity disagreed
	// with the one kept. A single project's graph can still collide with itself
	// when two nodes mint the same id.
	Conflicts []string `json:"conflicts,omitempty"`
}

// ProjectFeatureDesignSummaries reads one project's optional design graph. It
// returns whether the artifact exists and, when it exists but cannot be
// projected, why — never an error. agent_context is an orientation surface that
// predates the design graph; a stale file under .gen must not withhold the
// document an agent actually asked for.
func ProjectFeatureDesignSummaries(projectDir string) (features []FeatureDesignSummary, present bool, unreadable string) {
	artifact := filepath.Join(projectDir, ".gen", filepath.FromSlash(featureproto.DesignGraphArtifact))
	graph, found, reason := readProjectDesignGraph(artifact)
	if !found || reason != "" {
		return nil, found, reason
	}
	return summarizeProjectFeatureGraph(graph), true, ""
}

// readProjectDesignGraph parses one graph artifact, distinguishing "absent"
// from "present but unusable" so the caller can report the difference instead
// of collapsing both into an empty answer.
func readProjectDesignGraph(artifact string) (*featureproto.DesignGraph, bool, string) {
	data, err := os.ReadFile(artifact) //nolint:gosec // a project directory plus one protocol artifact name
	if os.IsNotExist(err) {
		return nil, false, ""
	}
	if err != nil {
		// *PathError repeats the absolute path the caller already reports.
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return nil, true, "read design graph: " + err.Error()
	}
	graph, err := featureproto.ParseDesignGraph(data)
	if err != nil {
		return nil, true, "parse design graph: " + err.Error()
	}
	return graph, true, ""
}

// summarizeProjectFeatureGraph merges the graph's semantic feature
// declarations. It never fails: a node that does not carry the feature:<id>
// identity is skipped, and an identity declared twice with different semantics
// keeps its first declaration in node order while naming the divergence in
// Conflicts. Orientation must stay available — one drifted declaration cannot
// be allowed to make every other feature in the project invisible.
func summarizeProjectFeatureGraph(graph *featureproto.DesignGraph) []FeatureDesignSummary {
	type aggregate struct {
		summary   FeatureDesignSummary
		projects  map[string]bool
		conflicts map[string]bool
	}
	byID := make(map[string]*aggregate)
	for _, node := range graph.Nodes {
		if node.Kind != featureproto.DesignNodeFeature {
			continue
		}
		id := strings.TrimPrefix(node.ID, "feature:")
		if !strings.HasPrefix(node.ID, "feature:") || id == "" {
			continue
		}
		candidate := FeatureDesignSummary{
			ID: id, Name: node.Name,
			Outcome: node.Properties["outcome"], Owner: node.Properties["owner"],
		}
		current := byID[id]
		if current == nil {
			current = &aggregate{
				summary:   candidate,
				projects:  make(map[string]bool),
				conflicts: make(map[string]bool),
			}
			byID[id] = current
		} else if current.summary.Name != candidate.Name ||
			current.summary.Outcome != candidate.Outcome ||
			current.summary.Owner != candidate.Owner {
			current.conflicts[graph.Project] = true
		}
		current.projects[graph.Project] = true
	}

	features := make([]FeatureDesignSummary, 0, len(byID))
	for _, entry := range byID {
		for project := range entry.projects {
			entry.summary.Projects = append(entry.summary.Projects, project)
		}
		sort.Strings(entry.summary.Projects)
		for project := range entry.conflicts {
			entry.summary.Conflicts = append(entry.summary.Conflicts, project)
		}
		sort.Strings(entry.summary.Conflicts)
		features = append(features, entry.summary)
	}
	sort.Slice(features, func(i, j int) bool { return features[i].ID < features[j].ID })
	return features
}
