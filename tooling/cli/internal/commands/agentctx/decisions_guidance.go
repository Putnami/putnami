package agentctx

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// decisionsPlaceholder is replaced with the workspace's settled decisions
// wherever the generated guidance carries them.
const decisionsPlaceholder = "{{DECISIONS}}"

// Settled decisions reach an agent where they bind.
//
// A workspace keeps one decisions.json at its root for the decisions every
// project follows, and one per project directory for that project's own. The
// generated block names the decisions.json files in one line and lists no
// decision. The line appears with a registry's first decision and goes with its
// last, so those two changes regenerate AGENTS.md and CLAUDE.md; every other
// decision change leaves them as they were. An agent reads the decisions in the
// files the line names.
//
// The derivation reads committed documents and falls back silently to nothing
// when one is absent or unusable. The guidance block is written into files a
// workspace owns, so a malformed registry must leave the block exactly as it
// was rather than fail a `putnami context generate`. `decisions-validate` is
// where a bad registry is reported; this derivation only quotes good ones.

// ProjectDecisions is the settled decisions of one project, from the
// decisions.json in its directory.
type ProjectDecisions struct {
	// Registry is the workspace-relative registry that holds them; a change
	// to one of them is an edit to this file.
	Registry string `json:"registry"`
	// Decisions are the registry's entries in canonical order. EVERY decision
	// is returned, not only the review-only ones: an agent that can see a
	// check exists does not spend a cycle proposing a change the gate will
	// reject.
	Decisions []featureproto.Decision `json:"decisions"`
}

// readDecisionRegistry reads the registry whose scope is the workspace-relative
// directory scope, in canonical order so the same registry always renders the
// same bytes whatever order it was authored in. It returns nil when the
// registry is absent, unusable or empty.
func readDecisionRegistry(wsRoot, scope string) *featureproto.DecisionRegistry {
	data, err := os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(featureproto.DecisionRegistryPath(scope))))
	if err != nil {
		return nil
	}
	registry, _ := featureproto.ParseAndValidateDecisionRegistry(data)
	if registry == nil || len(registry.Decisions) == 0 {
		return nil
	}
	return featureproto.CanonicalDecisionRegistry(registry)
}

// projectDecisions is one project's settled decisions, or nil when its
// directory holds no usable registry. The workspace root is not a project
// scope: its decisions already travel in the guidance.
func projectDecisions(wsRoot, projectPath string) *ProjectDecisions {
	projectPath = filepath.ToSlash(projectPath)
	if featureproto.DecisionRegistryPath(projectPath) == featureproto.DecisionsFilename {
		return nil
	}
	registry := readDecisionRegistry(wsRoot, projectPath)
	if registry == nil {
		return nil
	}
	return &ProjectDecisions{Registry: featureproto.DecisionRegistryPath(projectPath), Decisions: registry.Decisions}
}

// decisionsGuidanceForWorkspace renders the one block line that names the
// decisions.json files. It lists no decision.
//
// When no registry carries a decision, the placeholder renders NOTHING: no
// heading, no dangling bullet, and a block byte-identical to the one a
// workspace without a registry already has. The line also appears when only a
// project registry exists, because that registry binds its project.
func decisionsGuidanceForWorkspace(wsRoot string) string {
	if readDecisionRegistry(wsRoot, "") == nil && len(projectDecisionIndex(wsRoot)) == 0 {
		return ""
	}
	return "- Settled decisions are recorded in `" + featureproto.DecisionsFilename +
		"` files: the root one binds every project, a project's own binds that project. Read the file before you change a settled choice, and never re-decide one: to change a settled value, change the decision in its file in the same pull request.\n"
}

type projectDecisionEntry struct {
	registry string
	count    int
}

// projectDecisionIndex lists every project registry that carries a decision,
// sorted by location. A workspace that cannot be loaded lists none: the
// guidance then carries the root decisions alone rather than failing.
func projectDecisionIndex(wsRoot string) []projectDecisionEntry {
	ws, err := workspace.Load(wsRoot)
	if err != nil || ws == nil {
		return nil
	}
	seen := make(map[string]bool)
	var entries []projectDecisionEntry
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		decisions := projectDecisions(wsRoot, project.Path)
		if decisions == nil || seen[decisions.Registry] {
			continue
		}
		seen[decisions.Registry] = true
		entries = append(entries, projectDecisionEntry{registry: decisions.Registry, count: len(decisions.Decisions)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].registry < entries[j].registry })
	return entries
}

// applyDecisions substitutes the rendered decisions into a guidance template.
func applyDecisions(template string, decisions string) string {
	return strings.ReplaceAll(template, decisionsPlaceholder, decisions)
}
