// Package agentctx: the read-only MCP `agent_context` aggregator.
//
// BuildAgentContextResult freshly aggregates one project's committed facts into a
// gate-passed agentcontext.Document entirely in memory — no subprocess, no
// dependency on the on-disk artifact — and reports whether the ephemeral
// `<project>/.gen/agent-context.json` convenience artifact is present and byte-
// identical to that fresh build. It shares buildPackedDocument with `context
// pack`, so the MCP tool can never surface an unsafe/invalid document (a gate
// failure is the same exit-2 classified error the CLI returns) and its notion of
// "fresh" is exactly the CLI's notion of "not drifted".
//
// The MCP server reaches this builder through a dependency-injected closure on
// mcp.Options, wired by the CLI shell (internal/cli/mcp_serve.go). internal/mcp
// must never import internal/commands: MCP is an adapter
// over engine.Run, and internal/engine imports this package for the production
// preflight gate, so an mcp → commands edge would close a cycle. The closure
// returns the already-shaped result as `any`, so the mcp package needs no
// commands type.
package agentctx

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	agentcontext "go.putnami.dev/protocol/agentcontext"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// AgentContextResult is the MCP agent_context payload: the freshly aggregated,
// gate-passed document plus the freshness of the on-disk convenience artifact.
type AgentContextResult struct {
	Document       *agentcontext.Document      `json:"document"`
	DiskArtifact   DiskArtifactStatus          `json:"diskArtifact"`
	DesignArtifact FeatureDesignArtifactStatus `json:"designArtifact"`
	Features       []FeatureDesignSummary      `json:"features,omitempty"`
	// Decisions are the project's own settled decisions, from the
	// decisions.json in its directory. The workspace-wide ones travel in the
	// generated guidance instead.
	Decisions *ProjectDecisions `json:"decisions,omitempty"`
}

// DiskArtifactStatus reports the state of an ephemeral, gitignored convenience
// artifact relative to the document just built in memory — the
// `<project>/.gen/agent-context.json` written by `context pack` here, and the
// `.putnami/context-map/repo-map.json` written by `context map` for
// workspace_map. Every MCP answer is built in memory, so this is a report about
// the file, never a dependency of the answer.
type DiskArtifactStatus struct {
	Path    string `json:"path"`    // workspace-relative artifact path
	Present bool   `json:"present"` // the file exists on disk
	Fresh   bool   `json:"fresh"`   // present AND its bytes equal the in-memory build
}

// FeatureDesignArtifactStatus makes the optional build projection explicit.
// Unlike the deterministic agent-context document it is not a committed wire
// artifact, so consumers see presence and compatibility rather than an invented
// freshness guarantee.
type FeatureDesignArtifactStatus struct {
	Path          string `json:"path"`
	Present       bool   `json:"present"`
	Compatibility string `json:"compatibility,omitempty"`
	// Unreadable explains why a present artifact could not be projected — a
	// stale compatibility marker, a truncated write, an unreadable file. It is
	// reported, never returned as an error: orientation predates the design
	// graph and must not depend on the freshness of a gitignored build output.
	Unreadable string `json:"unreadable,omitempty"`
}

// BuildAgentContextResult loads the workspace, resolves projectSelector (id or
// name), builds + gates the document in-memory (NO subprocess, NO disk
// dependency), and compares it byte-for-byte to the on-disk artifact to report
// freshness. It reuses buildPackedDocument, so a structural or publish-safety
// failure returns the same exit-2 classified error `context pack` would and no
// unsafe document is ever returned. The document's provenance stamps the
// workspace revision resolved here.
//
// cliVersion must be the SAME version `context pack` stamped into provenance, or
// the on-disk artifact always reads as stale — the caller threads the running
// CLI version through so the two agree.
func BuildAgentContextResult(wsRoot, cliVersion, projectSelector string) (AgentContextResult, error) {
	// workspace.Load memoizes the workspace for the process lifetime. The CLI is a
	// one-shot process, but the MCP server is long-lived: without invalidating,
	// the first request's project metadata (main, dependencies, tags from
	// putnami.json / go.mod) would be served for the rest of the session, so an
	// edit made after the first call would return a stale document AND report
	// fresh: true against an equally stale on-disk artifact. Drop the memo so this
	// per-request aggregation genuinely reflects the current workspace.
	workspace.InvalidateLoadCache(wsRoot)
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return AgentContextResult{}, fmt.Errorf("load workspace: %w", err)
	}
	proj := shared.ResolveProjectSelector(ws, projectSelector)
	if proj == nil {
		return AgentContextResult{}, cmderr.NotFoundf("project not found: %s", projectSelector)
	}
	revision := resolveWorkspaceRevision(wsRoot)

	doc, rel, freshBytes, err := buildPackedDocument(ws, proj, revision, cliVersion)
	if err != nil {
		return AgentContextResult{}, err
	}

	status := DiskArtifactStatus{Path: rel}
	onDisk, readErr := os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(rel)))
	switch {
	case readErr == nil:
		status.Present = true
		status.Fresh = bytes.Equal(onDisk, freshBytes)
	case os.IsNotExist(readErr):
		// Present/Fresh stay false: the ephemeral artifact was never packed.
	default:
		return AgentContextResult{}, fmt.Errorf("read agent-context artifact %s: %w", rel, readErr)
	}

	projectDir := filepath.Join(wsRoot, filepath.FromSlash(proj.Path))
	features, designPresent, designUnreadable := ProjectFeatureDesignSummaries(projectDir)
	designRel := filepath.ToSlash(filepath.Join(
		filepath.FromSlash(proj.Path), ".gen", filepath.FromSlash(featureproto.DesignGraphArtifact),
	))
	designStatus := FeatureDesignArtifactStatus{
		Path:       designRel,
		Present:    designPresent,
		Unreadable: designUnreadable,
	}
	if designPresent && designUnreadable == "" {
		designStatus.Compatibility = featureproto.DesignGraphCompatibility
	}

	return AgentContextResult{
		Document:       doc,
		DiskArtifact:   status,
		DesignArtifact: designStatus,
		Features:       features,
		Decisions:      projectDecisions(wsRoot, proj.Path),
	}, nil
}
