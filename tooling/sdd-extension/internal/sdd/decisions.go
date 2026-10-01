package sdd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The decision gate of `validate-workspace`.
//
// A workspace states what it has settled in decisions.json registries: the
// one at the root for decisions every project follows, and one per project
// directory for that project's own. This engine reads every registry, proves
// every decision that carries a check against the files the check's own globs
// name inside the registry's directory, and fails the task naming the decision
// — its id, its statement, the date it was settled and the registry that holds
// it — rather than emitting a lint message a reader cannot act on.
//
// Two properties of the verdict are deliberate.
//
// It runs over the CURRENT WORKTREE ONLY and reads no git history, like
// `architecture-validate` and `specs-ratchet-validate`: the evidence is the
// files as they are, so the reviewed change a registry demands is literally
// an edit to that registry in the same diff.
//
// It is UNCACHEABLE, and unlike those two that is not a shortcut. A check's
// read set is whatever its own `files` globs name, and those globs are authored
// per repository, so no static task-input pattern can cover them. An
// under-declared key does not merely miss a change: it serves a stale verdict
// while claiming to have checked, which is the one failure a validation job
// must not have. That is the same reasoning `features-validate` states (D8),
// and it is stated the same way in the task manifest.
//
// Decisions are SETTLED, not ratcheted, so there is no verification-mode knob.
// `architecture` has one because a workspace adopts a graph gradually; a
// decision that a repository has written down and dated is either held or
// re-decided, and a "report" mode would be the silent re-decision this whole
// gate exists to prevent.

// excludedDecisionDirectories are never descended into when the matched-file
// set is walked. They are the same exclusions project discovery uses:
// generated output, vendored code, and every dot directory (which covers
// `.git`, `.gen` and `.putnami`). A verdict must be a function of the
// committed tree, and a generated copy of a file would make the same registry
// answer differently before and after a build.
var excludedDecisionDirectories = map[string]bool{
	"node_modules": true,
	"vendor":       true,
}

// maximumDecisionMatches bounds one check's matched set. A glob that matches
// more files than this is refused rather than read: an unbounded read set in a
// gate task is a timeout waiting for the repository that grows into it.
const maximumDecisionMatches = 4096

// DecisionsReport is the typed data of one decision judgment.
type DecisionsReport struct {
	// RegistryPresent reports whether any committed registry exists. Absence
	// is adoption, never a failure — the same rule as an absent
	// specs.baseline.json.
	RegistryPresent bool `json:"registryPresent"`
	// Registries are the workspace-relative locations of the registries read,
	// in sorted order.
	Registries []string `json:"registries,omitempty"`
	// Summary is the deterministic accounting of the run.
	Summary DecisionsSummary `json:"summary"`
	// Findings are the violations, one per violating file, each anchored on
	// that file's workspace-relative path.
	Findings []DecisionFinding `json:"findings,omitempty"`
	// Diagnostics carries the registries' own errors.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// DecisionsSummary is what a run states about the registries it read.
type DecisionsSummary struct {
	// Decisions counts the committed entries across every registry.
	Decisions int `json:"decisions"`
	// Enforced counts the entries validate proved.
	Enforced int `json:"enforced"`
	// ReviewOnly counts the entries only a reviewer can hold.
	ReviewOnly int `json:"reviewOnly"`
	// FilesChecked counts the distinct files the enforced checks read.
	FilesChecked int `json:"filesChecked"`
	// Violations counts the findings.
	Violations int `json:"violations"`
}

// DecisionFinding is one violated decision in one file.
type DecisionFinding struct {
	// Decision is the violated decision's stable id.
	Decision string `json:"decision"`
	// Registry is the workspace-relative registry that holds the decision.
	Registry string `json:"registry"`
	// Path is the workspace-relative file that violates it.
	Path string `json:"path"`
	// Detail is what was found, in the file's own vocabulary.
	Detail string `json:"detail"`
	// Message is the whole sentence published as a diagnostic.
	Message string `json:"message"`
}

// scopedDecisionRegistry is one parsed registry and the directory it governs.
type scopedDecisionRegistry struct {
	// scope is the slash-separated workspace-relative directory, "" for the
	// workspace root.
	scope    string
	path     string
	registry *featureproto.DecisionRegistry
}

// decisionScopes lists the directories a registry may sit in: the workspace
// root, then every project directory, sorted and without duplicates. A
// registry anywhere else governs nothing a reader can name, so it is not read.
func decisionScopes(ws *workspace.Workspace) []string {
	seen := map[string]bool{"": true}
	scopes := []string{""}
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		scope := workspace.CleanWorkspacePath(project.Path)
		if seen[scope] {
			continue
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes
}

// readCommittedDecisions loads one scope's registry. Absence is
// (nil, false, nil): adoption, with nothing settled in that scope yet.
func readCommittedDecisions(root, registryPath string) (*featureproto.DecisionRegistry, bool, []diag.Diagnostic) {
	data, err := readOptionalBoundedRegularFile(filepath.Join(root, filepath.FromSlash(registryPath)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeDecisionsUnreadable,
			registryPath, "read committed decision registry: %v", err)}
	}
	registry, findings := featureproto.ParseAndValidateDecisionRegistry(data)
	sourced := make([]diag.Diagnostic, 0, len(findings))
	for _, finding := range findings {
		finding.Field = documentField(registryPath, finding.Field)
		sourced = append(sourced, finding)
	}
	return registry, true, sourced
}

// documentField anchors a diagnostic on the committed document it came from,
// in the "<path>#<member>" convention discovery and the protocol validators
// already share, so the CLI shows WHICH document failed.
func documentField(document, member string) string {
	if member == "" {
		return document
	}
	return document + "#" + member
}

func unusableDecisions(report DecisionsReport) (DecisionsReport, error) {
	return report, WithResultData(protocolcli.Classify(
		fmt.Errorf("decisions: a committed %s is unusable", featureproto.DecisionsFilename),
		protocolcli.ErrInvalidConfig), report)
}

// BuildDecisionsResult is the whole of the workspace-scoped decision check.
//
// The order is the contract. Every registry validates itself first, then ids
// are proved unique across registries, and the run fails closed on any error:
// a registry that cannot be read whole is not a registry with fewer
// decisions, it is a policy nobody can state, and reporting the entries that
// happened to parse would claim a gate that is not there. Only then are the
// checks proved against the worktree.
func BuildDecisionsResult(ws *workspace.Workspace) (DecisionsReport, error) {
	var report DecisionsReport
	var registries []scopedDecisionRegistry
	for _, scope := range decisionScopes(ws) {
		registryPath := featureproto.DecisionRegistryPath(scope)
		registry, present, findings := readCommittedDecisions(ws.Root, registryPath)
		report.Diagnostics = append(report.Diagnostics, findings...)
		if !present {
			continue
		}
		report.RegistryPresent = true
		report.Registries = append(report.Registries, registryPath)
		if registry != nil {
			registries = append(registries, scopedDecisionRegistry{scope: scope, path: registryPath, registry: registry})
		}
	}
	sort.Strings(report.Registries)
	if diag.HasErrors(report.Diagnostics) {
		return unusableDecisions(report)
	}

	// An id is what a failure message and a pull request cite, so two
	// registries answering to the same id make the citation ambiguous. The
	// first registry in scope order keeps the id; every later one is named.
	owner := make(map[string]string)
	for _, scoped := range registries {
		for _, decision := range scoped.registry.Decisions {
			if first, taken := owner[decision.ID]; taken {
				report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidDecisionRegistry,
					documentField(scoped.path, decision.ID+".id"),
					"decision id %s is already settled in %s; ids are unique across every registry of a workspace", decision.ID, first))
				continue
			}
			owner[decision.ID] = scoped.path
		}
	}

	// An `adr` link is justification, and a link that resolves to nothing is
	// justification nobody can read. It is a filesystem fact, so it is checked
	// here rather than in the protocol validator, relative to the registry.
	for _, scoped := range registries {
		report.Summary.Decisions += len(scoped.registry.Decisions)
		for _, decision := range scoped.registry.Decisions {
			if decision.ADR == "" {
				continue
			}
			link := path.Join(scoped.scope, decision.ADR)
			if info, err := os.Lstat(filepath.Join(ws.Root, filepath.FromSlash(link))); err != nil || !info.Mode().IsRegular() {
				report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidDecisionRegistry,
					documentField(scoped.path, decision.ID+".adr"),
					"decision %s links %s, which is not a file in this worktree", decision.ID, link))
			}
		}
	}
	if diag.HasErrors(report.Diagnostics) {
		sortDiagnostics(report.Diagnostics)
		return unusableDecisions(report)
	}

	var files []string
	checked := make(map[string]bool)
	for _, scoped := range registries {
		for _, decision := range scoped.registry.Decisions {
			if decision.IsReviewOnly() {
				report.Summary.ReviewOnly++
				continue
			}
			report.Summary.Enforced++
			if files == nil {
				// The walk happens at most once per run, and only when a
				// decision actually carries a check: a review-only registry
				// reads no tree.
				var walkFindings []diag.Diagnostic
				files, walkFindings = walkDecisionFiles(ws.Root)
				report.Diagnostics = append(report.Diagnostics, walkFindings...)
			}
			matched, matchFindings := matchDecisionFiles(files, scoped, decision)
			report.Diagnostics = append(report.Diagnostics, matchFindings...)
			for _, relative := range matched {
				checked[relative] = true
				data, err := readOptionalBoundedRegularFile(filepath.Join(ws.Root, filepath.FromSlash(relative)))
				if err != nil {
					// Unreadable is fail-closed for the same reason unparseable
					// is: a decision must not be defeated by making its evidence
					// unavailable.
					report.Findings = append(report.Findings, decisionFinding(scoped.path, decision, relative,
						fmt.Sprintf("the file could not be read: %v", err)))
					continue
				}
				outcome := featureproto.EvaluateDecisionCheck(decision.Check, data)
				if outcome.Violated {
					report.Findings = append(report.Findings, decisionFinding(scoped.path, decision, relative, outcome.Detail))
				}
			}
		}
	}
	report.Summary.FilesChecked = len(checked)
	report.Summary.Violations = len(report.Findings)
	sortDiagnostics(report.Diagnostics)
	sort.Slice(report.Findings, func(i, j int) bool {
		if report.Findings[i].Decision != report.Findings[j].Decision {
			return report.Findings[i].Decision < report.Findings[j].Decision
		}
		return report.Findings[i].Path < report.Findings[j].Path
	})

	if diag.HasErrors(report.Diagnostics) {
		return unusableDecisions(report)
	}
	if len(report.Findings) > 0 {
		violated := make([]string, 0, len(report.Findings))
		for _, finding := range report.Findings {
			violated = append(violated, finding.Decision)
		}
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("decisions: %d violation(s) of settled decisions (%s)",
				len(report.Findings), strings.Join(dedupeSorted(violated), ", ")),
			protocolcli.ErrInvalidConfig), report)
	}
	return report, nil
}

func decisionFinding(registry string, decision featureproto.Decision, path, detail string) DecisionFinding {
	return DecisionFinding{
		Decision: decision.ID,
		Registry: registry,
		Path:     path,
		Detail:   detail,
		Message:  decision.ViolationMessage(registry, path, detail),
	}
}

// matchDecisionFiles selects the workspace files one decision's globs name
// inside its registry's directory, as sorted workspace-relative paths with no
// duplicate, so two runs over the same tree read the same files in the same
// order and report the same findings in the same sequence.
//
// Zero matches is SATISFIED and produces nothing: a scope with no such file
// cannot violate the decision, and turning "nothing to check" into a failure
// would make the shared format unusable outside the repository that authored
// the glob.
func matchDecisionFiles(files []string, scoped scopedDecisionRegistry, decision featureproto.Decision) ([]string, []diag.Diagnostic) {
	matched := make([]string, 0)
	for _, relative := range files {
		inScope, ok := featureproto.DecisionScopeRelative(scoped.scope, relative)
		if !ok {
			continue
		}
		for _, glob := range decision.Check.Files {
			if featureproto.MatchDecisionFileGlob(inScope, glob) {
				matched = append(matched, relative)
				break
			}
		}
	}
	if len(matched) > maximumDecisionMatches {
		return nil, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeInvalidDecisionRegistry,
			documentField(scoped.path, decision.ID+".check.files"),
			"decision %s matches %d files, beyond the bounded limit of %d; narrow its globs",
			decision.ID, len(matched), maximumDecisionMatches)}
	}
	sort.Strings(matched)
	return matched, nil
}

// walkDecisionFiles lists every regular workspace file a check may match, as
// sorted slash-separated workspace-relative paths.
//
// Directory symlinks are not followed and file symlinks are left out: a
// symlink's target may sit outside the worktree, and a verdict about a settled
// decision must be a function of the committed bytes rather than of what a
// developer's machine happens to link.
func walkDecisionFiles(root string) ([]string, []diag.Diagnostic) {
	paths := make([]string, 0)
	var diagnostics []diag.Diagnostic
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeDecisionsUnreadable, workspaceRelative(root, current),
				"walk workspace files for the decision registry: %v", walkErr))
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if current == root {
			return nil
		}
		if entry.IsDir() {
			if excludedDecisionDirectories[entry.Name()] || strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		paths = append(paths, workspaceRelative(root, current))
		return nil
	})
	if err != nil {
		diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeDecisionsUnreadable, "workspace",
			"walk workspace files for the decision registry: %v", err))
	}
	sort.Strings(paths)
	return paths, diagnostics
}
