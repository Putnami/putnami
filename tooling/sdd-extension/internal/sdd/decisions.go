package sdd

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The decision gate of `validate-workspace`.
//
// A workspace states what it has settled in decision registries: the one at
// the root for decisions every project follows, and one per project directory
// for that project's own. A registry is a decisions/ directory with one
// <id>.json file per decision, or one decisions.json file
// (featureproto.ReadScopeDecisions reads both). This engine reads every
// registry, proves every decision that carries a check against the files the
// check's own globs name inside the scope directory, and fails the task naming
// the decision — its id, its statement, the date it was settled and the file
// that holds it — rather than emitting a lint message a reader cannot act on.
//
// Two properties of the verdict are deliberate.
//
// It runs over the CURRENT WORKTREE ONLY and reads no git history, like
// `architecture-validate` and `specs-ratchet-validate`: the evidence is the
// files as they are, so the reviewed change a registry demands is literally
// an edit to that registry in the same diff.
//
// A check's read set is whatever its own `files` globs name, and those globs
// are authored per repository, so no narrow task-input pattern can cover them.
// The task therefore reads the workspace through its candidate cut
// (worktree.go) and is cached on the input `git:**`, which holds every file a
// glob can match: a registry, an ADR it links, and every file a check reads
// move the key, and an ignored or generated file is neither read nor keyed.
//
// Decisions are SETTLED, not ratcheted, so there is no verification-mode knob.
// `architecture` has one because a workspace adopts a graph gradually; a
// decision that a repository has written down and dated is either held or
// re-decided, and a "report" mode would be the silent re-decision this whole
// gate exists to prevent.

// excludedDecisionDirectories are never descended into when the matched-file
// set is listed. They are the same exclusions project discovery uses:
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
	// Registry is the workspace-relative file that holds the decision: a
	// decisions/<id>.json file or a decisions.json registry.
	Registry string `json:"registry"`
	// Path is the workspace-relative file that violates it.
	Path string `json:"path"`
	// Detail is what was found, in the file's own vocabulary.
	Detail string `json:"detail"`
	// Message is the whole sentence published as a diagnostic.
	Message string `json:"message"`
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
		scope := wsproto.CleanWorkspacePath(project.Path)
		if seen[scope] {
			continue
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes
}

// decisionFiles reads a scope's registry, in either form, through the
// worktree: the candidate cut inside a Git work tree, the disk outside one.
type decisionFiles struct{ tree *worktree }

func (f decisionFiles) ReadFile(name string) ([]byte, error) {
	return f.tree.readRegular(name)
}

func (f decisionFiles) ReadDir(dir string) ([]string, error) {
	return f.tree.regularFilesIn(dir)
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
	return report, protocolcli.WithResultData(protocolcli.Classify(
		errors.New("decisions: a committed decision registry is unusable"),
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
	tree, err := openWorktree(ws.Root)
	if err != nil {
		return report, fmt.Errorf("decisions: %w", err)
	}
	var registries []featureproto.ScopeDecisions
	for _, scope := range decisionScopes(ws) {
		scoped, findings := featureproto.ReadScopeDecisions(decisionFiles{tree: tree}, scope)
		report.Diagnostics = append(report.Diagnostics, findings...)
		if !scoped.Present {
			continue
		}
		report.RegistryPresent = true
		report.Registries = append(report.Registries, scoped.Location)
		registries = append(registries, scoped)
	}
	sort.Strings(report.Registries)
	if diag.HasErrors(report.Diagnostics) {
		sortDiagnostics(report.Diagnostics)
		return unusableDecisions(report)
	}

	// An id is what a failure message and a pull request cite, so two
	// decisions answering to the same id make the citation ambiguous. An id
	// that differs from another only in case is refused too: it names a
	// decision file that collides with the other one on a case-insensitive
	// file system. The first decision in scope order keeps the id; every later
	// one is named.
	owner := make(map[string]featureproto.SourcedDecision)
	for _, scoped := range registries {
		for _, sourced := range scoped.Decisions {
			id := sourced.Decision.ID
			if first, taken := owner[strings.ToLower(id)]; taken {
				field := documentField(sourced.Source, decisionMember(sourced, "id"))
				if first.Decision.ID == id {
					report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidDecisionRegistry, field,
						"decision id %s is already settled in %s; ids are unique across every registry of a workspace", id, first.Source))
				} else {
					report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidDecisionRegistry, field,
						"decision id %s differs only in case from %s, settled in %s; ids are unique, ignoring case, across every registry of a workspace",
						id, first.Decision.ID, first.Source))
				}
				continue
			}
			owner[strings.ToLower(id)] = sourced
		}
	}

	// An `adr` link is justification, and a link that resolves to nothing is
	// justification nobody can read. It is a filesystem fact, so it is checked
	// here rather than in the protocol validator, relative to the scope.
	for _, scoped := range registries {
		report.Summary.Decisions += len(scoped.Decisions)
		for _, sourced := range scoped.Decisions {
			if sourced.Decision.ADR == "" {
				continue
			}
			link := path.Join(scoped.Scope, sourced.Decision.ADR)
			if !tree.isRegular(link) {
				report.Diagnostics = append(report.Diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidDecisionRegistry,
					documentField(sourced.Source, decisionMember(sourced, "adr")),
					"decision %s links %s, which is not a file in this worktree", sourced.Decision.ID, link))
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
		for _, sourced := range scoped.Decisions {
			decision := sourced.Decision
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
				files, walkFindings = listDecisionFiles(tree)
				report.Diagnostics = append(report.Diagnostics, walkFindings...)
			}
			matched, matchFindings := matchDecisionFiles(files, scoped.Scope, sourced)
			report.Diagnostics = append(report.Diagnostics, matchFindings...)
			for _, relative := range matched {
				checked[relative] = true
				data, err := tree.readRegular(relative)
				if err != nil {
					// Unreadable is fail-closed for the same reason unparseable
					// is: a decision must not be defeated by making its evidence
					// unavailable.
					report.Findings = append(report.Findings, decisionFinding(sourced.Source, decision, relative,
						fmt.Sprintf("the file could not be read: %v", err)))
					continue
				}
				outcome := featureproto.EvaluateDecisionCheck(decision.Check, data)
				if outcome.Violated {
					report.Findings = append(report.Findings, decisionFinding(sourced.Source, decision, relative, outcome.Detail))
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
		return report, protocolcli.WithResultData(protocolcli.Classify(
			fmt.Errorf("decisions: %d violation(s) of settled decisions (%s)",
				len(report.Findings), strings.Join(dedupeSorted(violated), ", ")),
			protocolcli.ErrInvalidConfig), report)
	}
	return report, nil
}

// decisionMember names member of one decision inside the file that holds it:
// "<id>.<member>" in a decisions.json registry, "<member>" in a decision file
// of a decisions/ directory, which holds that one decision.
func decisionMember(sourced featureproto.SourcedDecision, member string) string {
	if path.Base(path.Dir(sourced.Source)) == featureproto.DecisionsDirname {
		return member
	}
	return sourced.Decision.ID + "." + member
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
// inside its scope directory, as sorted workspace-relative paths with no
// duplicate, so two runs over the same tree read the same files in the same
// order and report the same findings in the same sequence.
//
// Zero matches is SATISFIED and produces nothing: a scope with no such file
// cannot violate the decision, and turning "nothing to check" into a failure
// would make the shared format unusable outside the repository that authored
// the glob.
func matchDecisionFiles(files []string, scope string, sourced featureproto.SourcedDecision) ([]string, []diag.Diagnostic) {
	decision := sourced.Decision
	matched := make([]string, 0)
	for _, relative := range files {
		inScope, ok := featureproto.DecisionScopeRelative(scope, relative)
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
			documentField(sourced.Source, decisionMember(sourced, "check.files")),
			"decision %s matches %d files, beyond the bounded limit of %d; narrow its globs",
			decision.ID, len(matched), maximumDecisionMatches)}
	}
	sort.Strings(matched)
	return matched, nil
}

// listDecisionFiles lists every regular workspace file a check may match, as
// sorted slash-separated workspace-relative paths: the regular files of the
// candidate cut inside a Git work tree, and of the disk outside one.
//
// Directory symlinks are not followed and file symlinks are left out: a
// symlink's target may sit outside the worktree, and a verdict about a settled
// decision must be a function of the committed bytes rather than of what a
// developer's machine happens to link.
func listDecisionFiles(tree *worktree) ([]string, []diag.Diagnostic) {
	if tree.tree != nil {
		return tree.candidateRegularFiles(excludedDecisionDirectory), nil
	}
	return walkDecisionFiles(tree.root)
}

// walkDecisionFiles is listDecisionFiles outside a Git work tree: one walk of
// the disk.
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
			if excludedDecisionDirectory(entry.Name()) {
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
