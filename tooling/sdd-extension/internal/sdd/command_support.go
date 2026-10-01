package sdd

import (
	archproto "go.putnami.dev/protocol/architecture"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// This file is the engine's seam towards the command layer, and nothing else.
//
// `internal/sdd` decides; `cmd/putnami-sdd` shows. That split is why the CLI,
// the DAG jobs and the MCP tools cannot answer differently, and it is also why
// the human renderers live outside this package: a report is one value, and the
// several ways of printing it are not the engine's subject.
//
// A renderer nevertheless needs a handful of DERIVED facts that only the engine
// can compute — which authority a design node was reached under, which producer
// operations a generated client came from, the counts a snapshot implies. Those
// are re-derivations of engine knowledge, not presentation, so they stay here
// and are exported through the thin wrappers below rather than duplicated in
// the command layer. Duplicating them is how a printed count and a structured
// count start disagreeing about the same run.
//
// The wrappers are deliberately wrappers. The bodies below are the byte-for-byte
// copies the engine move brought over from `tooling/cli/internal/commands/sdd`, and renaming
// them in place would have made that copy unprovable by diff.

// SummarizeFeatures is the compact validation and human-summary projection of a
// snapshot: feature, requirement, evidence and verification-state counts.
func SummarizeFeatures(snapshot *featureengine.Snapshot) FeatureCounts {
	return summarizeFeatures(snapshot)
}

// SummarizeArchitecture is the compact repository projection of one evaluated
// architecture snapshot.
func SummarizeArchitecture(snapshot *archproto.Snapshot) ArchitectureCounts {
	return summarizeArchitecture(snapshot)
}

// DesignNodeAuthority reports the weakest authority any critical path reached
// the node under, defaulting to "exact" when no path ends at it. It is a
// property of the traversal, not of the node, which is why the renderer cannot
// read it off the node itself.
func DesignNodeAuthority(implementation FeatureDesignImplementation, nodeID string) string {
	return designNodeAuthority(implementation, nodeID)
}

// GeneratedClientOperationDetails renders the bounded, sorted list of producer
// operations a generated client was emitted from. The answer lives in the
// implementation's EDGES, so only a graph walk can produce it.
func GeneratedClientOperationDetails(implementation FeatureDesignImplementation, clientID string) string {
	return generatedClientOperationDetails(implementation, clientID)
}

// CompactFactProvenance returns the provenance a design node carries, or the
// one its declaring edge carries when the node itself is silent — the same
// resolution the agent-facing projection applies, so a printed source line and
// a structured one name the same file.
func CompactFactProvenance(
	implementation FeatureDesignImplementation,
	node featureproto.DesignNode,
) *featureproto.DesignProvenance {
	return compactFactProvenance(implementation, node)
}

// ResolveProjectSelector resolves a selector to one workspace member: an ID
// first when it is path-shaped, then a name, then a path-shaped ID. It is what
// `contracts generate|check` turns its single `--project` target into.
func ResolveProjectSelector(ws *workspace.Workspace, selector string) *workspace.Project {
	return resolveProjectSelector(ws, selector)
}
