package sdd

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"

	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The verdict policy gate of `validate-workspace`.
//
// A putnami.verdict-policy.json file states what each gap and each declaration
// change of a verdict costs (ADR 0008 of protocols/ci). This step checks every
// such file of the workspace in two ways:
//
//   - The ci protocol's strict parser checks the content: an unknown kind,
//     value, key or maturity, a map by maturity that does not hold modeled,
//     and a map on a key that concerns no feature. Each finding keeps the
//     protocol's code.
//   - This step checks the place, which the protocol does not know: a file
//     sits at the root, in a project directory, or in a scope directory. A
//     scope directory is a directory above a project whose putnami.json is a
//     scope manifest.
//
// A workspace without a policy file passes: the CI plane then applies its
// defaults.
//
// Inside a Git work tree the step reads the candidate cut (worktree.go), so an
// ignored file is never read. The listing never enters a directory that the
// cache key's file walk skips, nor the generated .gen tree, so the key holds
// every policy file the step reads.

// Verdict policy diagnostic codes. The content findings keep the ci
// protocol's codes.
const (
	// ErrorCodeVerdictPolicyLocation refuses a policy file that is not at the
	// root, in a project directory or in a scope directory.
	ErrorCodeVerdictPolicyLocation = "sdd.verdict_policy_location"
	// ErrorCodeVerdictPolicyUnreadable refuses a policy file the step cannot
	// read, such as a symbolic link or a file over the read bound.
	ErrorCodeVerdictPolicyUnreadable = "sdd.verdict_policy_unreadable"
)

// verdictPolicyDocumentField is the field the ci protocol gives a finding
// about a whole file.
const verdictPolicyDocumentField = "(document)"

// verdictPolicySkippedDirectories are the directories the listing never
// enters: the ones the cache key's file walk skips, and .gen.
var verdictPolicySkippedDirectories = map[string]bool{
	".gen":         true,
	".git":         true,
	".putnami":     true,
	"dist":         true,
	"node_modules": true,
	"out":          true,
	"vendor":       true,
}

// VerdictPolicyReport is the typed data of one verdict policy judgment.
type VerdictPolicyReport struct {
	// Policies lists the workspace-relative policy files that were found.
	Policies []string `json:"policies"`
	// Diagnostics carries every finding, anchored on its policy file.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// BuildVerdictPolicyResult validates every verdict policy file of the
// workspace. It fails on any error finding, and on a listing it cannot
// finish.
func BuildVerdictPolicyResult(ws *workspace.Workspace) (VerdictPolicyReport, error) {
	report := VerdictPolicyReport{Policies: []string{}}
	tree, err := openWorktree(ws.Root)
	if err != nil {
		return report, fmt.Errorf("verdict policy: %w", err)
	}
	files, findings := listVerdictPolicies(tree)
	report.Diagnostics = append(report.Diagnostics, findings...)
	places := newVerdictPolicyPlaces(ws)
	for _, file := range files {
		report.Policies = append(report.Policies, file)
		if dir := path.Dir(file); !places.allows(dir) {
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(ErrorCodeVerdictPolicyLocation, file,
				"a verdict policy sits at the workspace root, in a scope directory or in a project directory; %s is none of them", dir))
		}
		data, err := tree.readRegular(file)
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, diag.Errorf(ErrorCodeVerdictPolicyUnreadable, file,
				"read verdict policy: %v", err))
			continue
		}
		_, parsed := ciproto.ParseVerdictPolicyWithDiagnostics(file, data)
		for i := range parsed {
			if parsed[i].Field == verdictPolicyDocumentField {
				parsed[i].Field = ""
			}
		}
		appendPathDiagnostics(&report.Diagnostics, file, parsed)
	}
	sortDiagnostics(report.Diagnostics)
	if diag.HasErrors(report.Diagnostics) {
		return report, protocolcli.WithResultData(protocolcli.Classify(
			fmt.Errorf("verdict policy: %d finding(s) in the verdict policy files", len(report.Diagnostics)),
			protocolcli.ErrInvalidConfig), report)
	}
	return report, nil
}

// listVerdictPolicies lists every file named ciproto.VerdictPolicyFile as
// sorted slash paths relative to the root. Inside a Git work tree it lists the
// candidate cut, a symbolic link included, so the read refuses the link.
// Outside one it walks the disk, and a directory it cannot read is a finding.
func listVerdictPolicies(tree *worktree) ([]string, []diag.Diagnostic) {
	if tree.tree == nil {
		return walkVerdictPolicies(tree.root)
	}
	var files []string
	for _, rel := range tree.tree.Paths() {
		if path.Base(rel) == ciproto.VerdictPolicyFile && !inSkippedVerdictPolicyDirectory(rel) {
			files = append(files, rel)
		}
	}
	return files, nil
}

// inSkippedVerdictPolicyDirectory reports whether a directory of the slash
// path rel is one the listing never enters.
func inSkippedVerdictPolicyDirectory(rel string) bool {
	for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
		if verdictPolicySkippedDirectories[path.Base(dir)] {
			return true
		}
	}
	return false
}

// walkVerdictPolicies is listVerdictPolicies outside a Git work tree.
func walkVerdictPolicies(root string) ([]string, []diag.Diagnostic) {
	var files []string
	var findings []diag.Diagnostic
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			findings = append(findings, diag.Errorf(ErrorCodeVerdictPolicyUnreadable, workspaceRelative(root, current),
				"list the verdict policy files: %v", walkErr))
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if current == root {
			return nil
		}
		if entry.IsDir() {
			if verdictPolicySkippedDirectories[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == ciproto.VerdictPolicyFile {
			files = append(files, workspaceRelative(root, current))
		}
		return nil
	})
	if err != nil {
		findings = append(findings, diag.Errorf(ErrorCodeVerdictPolicyUnreadable, "workspace",
			"list the verdict policy files: %v", err))
	}
	sort.Strings(files)
	return files, findings
}

// verdictPolicyPlaces knows the directories that may hold a policy file.
type verdictPolicyPlaces struct {
	root string
	// projects holds each project directory.
	projects map[string]bool
	// above holds each directory above a project and below the root: the
	// directories that may be a scope.
	above map[string]bool
}

func newVerdictPolicyPlaces(ws *workspace.Workspace) verdictPolicyPlaces {
	places := verdictPolicyPlaces{root: ws.Root, projects: map[string]bool{}, above: map[string]bool{}}
	for _, project := range ws.Projects {
		dir := wsproto.CleanWorkspacePath(project.Path)
		places.projects[dir] = true
		for parent := path.Dir(dir); parent != "." && parent != "/"; parent = path.Dir(parent) {
			places.above[parent] = true
		}
	}
	return places
}

// allows reports whether the slash directory dir may hold a policy file: the
// root, a project directory, or a directory above a project whose
// putnami.json is a scope manifest, as the workspace reads it.
func (p verdictPolicyPlaces) allows(dir string) bool {
	switch {
	case dir == "." || p.projects[dir]:
		return true
	case p.above[dir]:
		return wsproto.ReadScopeConfig(filepath.Join(p.root, filepath.FromSlash(dir))) != nil
	}
	return false
}
