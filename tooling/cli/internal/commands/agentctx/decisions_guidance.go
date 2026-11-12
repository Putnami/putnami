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
// A workspace keeps a registry at its root for the decisions every project
// follows, and one per project directory for that project's own. A registry
// is a decisions/ directory with one <id>.json file per decision, or one
// decisions.json file. The generated block names where the decisions are in
// one line and lists no decision. The line appears with a registry's first
// decision, goes with its last, and changes when a registry changes form, so
// only those changes regenerate AGENTS.md and CLAUDE.md; every other decision
// change leaves them as they were. An agent reads the decisions in the files
// the line names.
//
// The derivation reads committed documents and falls back silently to nothing
// when one is absent or unusable. The guidance block is written into files a
// workspace owns, so a malformed registry must leave the block exactly as it
// was rather than fail a `putnami context generate`. `decisions-validate` is
// where a bad registry is reported; this derivation only quotes good ones.

// ProjectDecisions is the settled decisions of one project, from the registry
// in its directory.
type ProjectDecisions struct {
	// Registry is the workspace-relative registry that holds them: the
	// project's decisions/ directory or its decisions.json.
	Registry string `json:"registry"`
	// Decisions are the registry's entries in canonical order. EVERY decision
	// is returned, not only the review-only ones: an agent that can see a
	// check exists does not spend a cycle proposing a change the gate will
	// reject.
	Decisions []featureproto.Decision `json:"decisions"`
}

// diskDecisionFiles reads a workspace's decision files from disk.
type diskDecisionFiles struct{ root string }

func (d diskDecisionFiles) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.root, filepath.FromSlash(name)))
}

func (d diskDecisionFiles) ReadDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(d.root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// readScopeDecisions reads the registry whose scope is the workspace-relative
// directory scope, in canonical order so the same registry always renders the
// same bytes whatever order it was authored in. It returns nil when the
// registry is absent, unusable or empty.
func readScopeDecisions(wsRoot, scope string) *featureproto.ScopeDecisions {
	scoped, _ := featureproto.ReadScopeDecisions(diskDecisionFiles{root: wsRoot}, scope)
	if len(scoped.Decisions) == 0 {
		return nil
	}
	return &scoped
}

// projectDecisions is one project's settled decisions, or nil when its
// directory holds no usable registry. The workspace root is not a project
// scope: its decisions already travel in the guidance.
func projectDecisions(wsRoot, projectPath string) *ProjectDecisions {
	projectPath = filepath.ToSlash(projectPath)
	if featureproto.DecisionRegistryPath(projectPath) == featureproto.DecisionsFilename {
		return nil
	}
	scoped := readScopeDecisions(wsRoot, projectPath)
	if scoped == nil {
		return nil
	}
	decisions := make([]featureproto.Decision, 0, len(scoped.Decisions))
	for _, sourced := range scoped.Decisions {
		decisions = append(decisions, sourced.Decision)
	}
	return &ProjectDecisions{Registry: scoped.Location, Decisions: decisions}
}

// The block line for each set of registry forms a workspace holds.
const (
	decisionsFilesLine = "- Settled decisions are recorded one per file in `decisions/<id>.json`: the root `decisions/` binds every project, a project's own binds that project. " +
		"Read the decision before you change a settled choice, and never re-decide one: to change a settled value, change its file in the same pull request. " +
		"Record a new decision in a new file named by a slug id.\n"
	decisionsRegistryLine = "- Settled decisions are recorded in `decisions.json` files: the root one binds every project, a project's own binds that project. " +
		"Read the file before you change a settled choice, and never re-decide one: to change a settled value, change the decision in its file in the same pull request.\n"
	decisionsMixedLine = "- Settled decisions are recorded in `decisions/<id>.json` files or in a `decisions.json` file: the root ones bind every project, a project's own bind that project. " +
		"Read the decision before you change a settled choice, and never re-decide one: to change a settled value, change the decision in its file in the same pull request. " +
		"Record a new decision in the form its directory already uses.\n"
)

// decisionsGuidanceForWorkspace renders the one block line that names where
// the decisions are. It lists no decision.
//
// When no registry carries a decision, the placeholder renders NOTHING: no
// heading, no dangling bullet, and a block byte-identical to the one a
// workspace without a registry already has. The line also appears when only a
// project registry exists, because that registry binds its project. Its
// wording follows the forms the registries use, so it never sends an agent to
// a form its workspace does not hold.
func decisionsGuidanceForWorkspace(wsRoot string) string {
	locations := projectDecisionIndex(wsRoot)
	if root := readScopeDecisions(wsRoot, ""); root != nil {
		locations = append(locations, projectDecisionEntry{registry: root.Location, count: len(root.Decisions)})
	}
	files, registries := false, false
	for _, entry := range locations {
		if strings.HasSuffix(entry.registry, featureproto.DecisionsFilename) {
			registries = true
		} else {
			files = true
		}
	}
	switch {
	case files && registries:
		return decisionsMixedLine
	case files:
		return decisionsFilesLine
	case registries:
		return decisionsRegistryLine
	default:
		return ""
	}
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
