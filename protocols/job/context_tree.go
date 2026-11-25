package job

// Tree facts, an optional v2 member.
//
// `tree` carries what Git says about the tree the job runs in, as the
// orchestrator read it once for the run: the directories Git ignores entirely,
// the paths the index tracks, the repository state and the project's release
// baseline. The rule that decides which job receives which fact is recorded
// with its rejected alternatives in doc/adr/0002-a-job-reads-the-tree-facts-its-key-holds.md:
// a task declares each fact as a runtime input, the orchestrator folds the fact
// into the task's cache key, and only then writes it here. A job never reads a
// fact its key does not hold.
//
// Each fact is optional on its own. An absent fact says the task did not
// declare it, or the orchestrator could not read it; a consumer then asks Git
// itself, as it did before the member existed. An empty listing is an answer:
// `[]` says Git tracks nothing, or ignores nothing, at or below the directory.

import (
	"fmt"
	"path"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Repository states: the closed vocabulary of TreeRepository.State.
const (
	// RepositoryUnmanaged: the workspace is outside every Git work tree.
	RepositoryUnmanaged = "unmanaged"
	// RepositoryNoCommit: the work tree has no commit at HEAD.
	RepositoryNoCommit = "no-commit"
	// RepositoryShallow: the clone is shallow, so its history and tags are
	// incomplete.
	RepositoryShallow = "shallow"
	// RepositoryComplete: a work tree with a commit and its whole history.
	RepositoryComplete = "complete"
)

// RepositoryStates is the closed repository-state vocabulary in canonical
// (sorted) order.
var RepositoryStates = []string{
	RepositoryComplete,
	RepositoryNoCommit,
	RepositoryShallow,
	RepositoryUnmanaged,
}

// Release baseline states: the closed vocabulary of ReleaseBaseline.State.
// They are the states the orchestrator's release-baseline runtime input keys
// (tooling/cli ADR 0062), read in this order: outside a work tree, without a
// commit, in a shallow clone, without a release of the line, tagged.
const (
	ReleaseBaselineUnmanaged = RepositoryUnmanaged
	ReleaseBaselineNoCommit  = RepositoryNoCommit
	ReleaseBaselineShallow   = RepositoryShallow
	ReleaseBaselineUntagged  = "untagged"
	ReleaseBaselineTagged    = "tagged"
)

// ReleaseBaselineStates is the closed release-baseline-state vocabulary in
// canonical (sorted) order.
var ReleaseBaselineStates = []string{
	ReleaseBaselineNoCommit,
	ReleaseBaselineShallow,
	ReleaseBaselineTagged,
	ReleaseBaselineUnmanaged,
	ReleaseBaselineUntagged,
}

// Tree is the set of tree facts the job's task declared, each the value its
// cache key holds. v2 only, and optional within v2: the orchestrator writes it
// only for a task that declares at least one fact.
type Tree struct {
	// Directory is the directory the listings cover, relative to the workspace
	// root in slash form: the project's path for a project task, "." for a
	// workspace task.
	Directory string `json:"directory"`
	// Repository is the repository state, present when the task declares the
	// `repository` runtime input.
	Repository *TreeRepository `json:"repository,omitempty"`
	// TrackedFiles are the paths the Git index tracks at or below Directory,
	// relative to the workspace root in slash form, sorted and unique. The
	// index is read alone, so a tracked file deleted from the work tree is
	// listed. Present when the task declares the `trackedFiles` runtime input.
	TrackedFiles []string `json:"trackedFiles,omitzero"`
	// IgnoredDirectories are the directories Git ignores entirely at or below
	// Directory (`git ls-files --others --ignored --exclude-standard
	// --directory`), relative to the workspace root in slash form, without a
	// trailing slash, sorted and unique. A directory that holds a tracked file
	// is not ignored entirely, and is not listed. Present when the task
	// declares the `ignoredDirectories` runtime input.
	IgnoredDirectories []string `json:"ignoredDirectories,omitzero"`
	// ReleaseBaseline is the project's release baseline, present when the
	// task declares the `releaseBaseline` runtime input.
	ReleaseBaseline *ReleaseBaseline `json:"releaseBaseline,omitempty"`
	// CandidateDirectories are the directories CandidateFiles covers,
	// relative to the workspace root in slash form ("." for the root), sorted
	// and unique: the task's directory, and each member of its dependency
	// closure when a closure input holds a `git:` pattern (doc/adr/0003).
	// Present exactly when CandidateFiles is.
	CandidateDirectories []string `json:"candidateDirectories,omitzero"`
	// CandidateFiles is the candidate cut at or below CandidateDirectories:
	// the paths a `git:` input enumerates (`git ls-files --cached --others
	// --exclude-standard`), before the disk is read, with their index modes.
	// Sorted by path and unique. Present when the task declares the
	// `candidateFiles` runtime input and the workspace is inside a work tree.
	CandidateFiles []CandidateFile `json:"candidateFiles,omitzero"`
	// WorkspaceCandidates names the file that holds the workspace's candidate
	// cut, written once per run, present when the task declares the
	// `workspaceCandidateFiles` runtime input and the workspace is inside a
	// work tree.
	WorkspaceCandidates *CandidateListing `json:"workspaceCandidates,omitempty"`
	// ChangeHistory is the history of the change since the run's impacted
	// baseline, present when the task declares the `changeHistory` runtime
	// input, its cache is disabled and the run has an impacted baseline.
	ChangeHistory *ChangeHistory `json:"changeHistory,omitempty"`
}

// Candidate modes: the closed vocabulary of CandidateFile.Mode. A tracked
// path carries its index mode; an untracked one, and one the index holds
// unmerged, carry no single mode.
const (
	CandidateModeRegular    = "100644"
	CandidateModeExecutable = "100755"
	CandidateModeSymlink    = "120000"
	CandidateModeSubmodule  = "160000"
	CandidateModeUnmerged   = "unmerged"
	CandidateModeUntracked  = "untracked"
)

// CandidateModes is the closed candidate-mode vocabulary in canonical (sorted)
// order.
var CandidateModes = []string{
	CandidateModeRegular,
	CandidateModeExecutable,
	CandidateModeSymlink,
	CandidateModeSubmodule,
	CandidateModeUnmerged,
	CandidateModeUntracked,
}

// CandidateFile is one path of the candidate cut.
type CandidateFile struct {
	// Path is relative to the workspace root, in slash form.
	Path string `json:"path"`
	// Mode is one of CandidateModes.
	Mode string `json:"mode"`
}

// CandidateListing names a file of the run that holds a candidate cut: the
// JSON array of CandidateFile EncodeCandidateFiles writes.
type CandidateListing struct {
	// File is the absolute path of the file, which the job reads and never
	// writes; the run removes it when it ends.
	File string `json:"file"`
	// Digest is the lowercase hex SHA-256 of the file's bytes, the value the
	// task's key holds.
	Digest string `json:"digest"`
}

// ChangeHistory is what the history says about the change since the run's
// impacted baseline.
type ChangeHistory struct {
	// Baseline is the run's impacted baseline, as the selection names it.
	Baseline string `json:"baseline"`
	// Shallow reports a shallow clone, whose history may stop before the
	// merge base: it then carries nothing else.
	Shallow bool `json:"shallow,omitempty"`
	// MergeBase is the merge base of Baseline and HEAD.
	MergeBase string `json:"mergeBase,omitempty"`
	// Commits are the commits of Baseline..HEAD, newest first, merges
	// included. Present, possibly empty, when the clone is not shallow.
	Commits []HistoryCommit `json:"commits,omitzero"`
	// WorkspaceConfig is putnami.workspace.json as MergeBase holds it at the
	// workspace's directory, absent when it holds none.
	WorkspaceConfig *string `json:"workspaceConfig,omitempty"`
}

// HistoryCommit is one commit message of the history.
type HistoryCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Body    string `json:"body,omitempty"`
}

// TreeRepository is the repository state of the workspace.
type TreeRepository struct {
	// State is one of RepositoryStates.
	State string `json:"state"`
}

// ReleaseBaseline is what Git says about the project's last release: the
// value the task's cache key holds (tooling/cli ADR 0062), plus the directory
// that holds the project as the release tag holds it.
type ReleaseBaseline struct {
	// State is one of ReleaseBaselineStates.
	State string `json:"state"`
	// Pattern is the tag pattern of the project's version line.
	Pattern string `json:"pattern"`
	// HasTags reports, for an untagged line, whether the repository has a tag
	// of any line.
	HasTags bool `json:"hasTags,omitempty"`
	// Tag is the tag of the line's last release, when State is tagged.
	Tag string `json:"tag,omitempty"`
	// Breaking reports, when State is tagged, whether a commit since Tag that
	// touches the project directory declares a breaking change.
	Breaking bool `json:"breaking,omitempty"`
	// Directory is the absolute path of a directory that holds the files Tag
	// holds at the project directory, which the job reads and never writes; the
	// run removes it when it ends. Present when the task also declares the
	// `releaseBaselineFiles` runtime input, State is tagged and the project
	// directory exists at Tag.
	Directory string `json:"directory,omitempty"`
}

// validateTree checks the tree facts.
//
// A PRESENT-but-null member, and a present-but-null listing, are neither an
// answer nor its absence. A tree without a fact is no member at all: the
// orchestrator writes one only for a task that declares a fact.
// treeListings is which listings the document carries, null included.
type treeListings struct {
	tracked, ignored, candidates, candidateDirectories bool
}

func validateTree(tree *Tree, present bool, listings treeListings) []diag.Diagnostic {
	present = present || tree != nil
	if !present {
		return nil
	}
	if tree == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "tree",
			"tree must be omitted or contain a non-null object")}
	}
	tracked := listings.tracked || tree.TrackedFiles != nil
	ignored := listings.ignored || tree.IgnoredDirectories != nil
	candidates := listings.candidates || tree.CandidateFiles != nil
	candidateDirectories := listings.candidateDirectories || tree.CandidateDirectories != nil
	var diags []diag.Diagnostic
	switch {
	case tree.Directory == "":
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, "tree.directory", "tree.directory is required"))
	case !cleanRelativePath(tree.Directory) && tree.Directory != ".":
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree.directory",
			"tree.directory must be a clean slash path relative to the workspace root, or \".\", got %q", tree.Directory))
	}
	if tree.Repository == nil && !tracked && !ignored && tree.ReleaseBaseline == nil && !candidates &&
		!candidateDirectories && tree.WorkspaceCandidates == nil && tree.ChangeHistory == nil {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree",
			"tree carries no fact; omit the member when the task declares none"))
	}
	if tree.Repository != nil && !oneOf(tree.Repository.State, RepositoryStates) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree.repository.state",
			"tree.repository.state is %q; want one of %s", tree.Repository.State, strings.Join(RepositoryStates, ", ")))
	}
	if tracked {
		diags = append(diags, validateTreeListing("tree.trackedFiles", tree.Directory, tree.TrackedFiles)...)
	}
	if ignored {
		diags = append(diags, validateTreeListing("tree.ignoredDirectories", tree.Directory, tree.IgnoredDirectories)...)
	}
	diags = append(diags, validateReleaseBaseline(tree.ReleaseBaseline)...)
	if candidates != candidateDirectories {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree.candidateFiles",
			"tree.candidateFiles and tree.candidateDirectories go together"))
	} else if candidates {
		diags = append(diags, validateCandidates(tree.CandidateDirectories, tree.CandidateFiles)...)
	}
	diags = append(diags, validateCandidateListing(tree.WorkspaceCandidates)...)
	diags = append(diags, validateChangeHistory(tree.ChangeHistory)...)
	return diags
}

// validateCandidates checks a candidate cut and the directories it covers.
func validateCandidates(directories []string, files []CandidateFile) []diag.Diagnostic {
	if directories == nil || files == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "tree.candidateFiles",
			"tree.candidateFiles and tree.candidateDirectories must be omitted or contain a non-null array")}
	}
	var diags []diag.Diagnostic
	if len(directories) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree.candidateDirectories",
			"tree.candidateDirectories names at least one directory"))
	}
	for i, directory := range directories {
		if directory != "." && !cleanRelativePath(directory) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, fmt.Sprintf("tree.candidateDirectories[%d]", i),
				"tree.candidateDirectories[%d] must be a clean slash path relative to the workspace root, or \".\", got %q", i, directory))
		}
		if i > 0 && directories[i-1] >= directory {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree.candidateDirectories",
				"tree.candidateDirectories must be sorted and free of duplicates; %q precedes %q", directories[i-1], directory))
		}
	}
	diags = append(diags, ValidateCandidateFiles("tree.candidateFiles", directories, files)...)
	return diags
}

// ValidateCandidateFiles checks a candidate cut: every path clean, relative
// and at or below one of directories, every mode one of CandidateModes, the
// paths sorted in byte order and free of duplicates.
func ValidateCandidateFiles(field string, directories []string, files []CandidateFile) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for i, file := range files {
		covered := false
		for _, directory := range directories {
			if atOrBelow(directory, file.Path) {
				covered = true
				break
			}
		}
		if !cleanRelativePath(file.Path) || !covered {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, fmt.Sprintf("%s[%d].path", field, i),
				"%s[%d].path must be a clean slash path relative to the workspace root at or below %s, got %q",
				field, i, strings.Join(directories, ", "), file.Path))
		}
		if !oneOf(file.Mode, CandidateModes) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, fmt.Sprintf("%s[%d].mode", field, i),
				"%s[%d].mode is %q; want one of %s", field, i, file.Mode, strings.Join(CandidateModes, ", ")))
		}
		if i > 0 && files[i-1].Path >= file.Path {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field,
				"%s must be sorted by path and free of duplicates; %q precedes %q", field, files[i-1].Path, file.Path))
		}
	}
	return diags
}

// validateCandidateListing checks the name of a candidate listing file.
func validateCandidateListing(listing *CandidateListing) []diag.Diagnostic {
	if listing == nil {
		return nil
	}
	var diags []diag.Diagnostic
	if !isAbsolutePath(listing.File) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree.workspaceCandidates.file",
			"tree.workspaceCandidates.file must be an absolute path, got %q", listing.File))
	}
	if !lowerHex(listing.Digest, 64) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree.workspaceCandidates.digest",
			"tree.workspaceCandidates.digest must be a lowercase hex SHA-256, got %q", listing.Digest))
	}
	return diags
}

// validateChangeHistory checks a change history: the baseline, and either a
// shallow clone that carries nothing else or the merge base and the commits.
func validateChangeHistory(history *ChangeHistory) []diag.Diagnostic {
	if history == nil {
		return nil
	}
	const field = "tree.changeHistory"
	var diags []diag.Diagnostic
	if strings.TrimSpace(history.Baseline) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, field+".baseline", "%s.baseline is required", field))
	}
	if history.Shallow {
		if history.MergeBase != "" || history.Commits != nil || history.WorkspaceConfig != nil {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field,
				"%s of a shallow clone carries no merge base, commit or workspace config", field))
		}
		return diags
	}
	if !commitID(history.MergeBase) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+".mergeBase",
			"%s.mergeBase must be a full commit ID, got %q", field, history.MergeBase))
	}
	if history.Commits == nil {
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, field+".commits",
			"%s.commits is required when the clone is not shallow; no commit is an empty array", field))
	}
	for i, commit := range history.Commits {
		if !commitID(commit.SHA) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, fmt.Sprintf("%s.commits[%d].sha", field, i),
				"%s.commits[%d].sha must be a full commit ID, got %q", field, i, commit.SHA))
		}
	}
	return diags
}

// commitID reports a full SHA-1 or SHA-256 commit ID in lowercase hex.
func commitID(value string) bool {
	return lowerHex(value, 40) || lowerHex(value, 64)
}

func lowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateTreeListing checks one listing: non-null, every path clean, relative
// and at or below the directory, sorted in byte order and free of duplicates.
func validateTreeListing(field, directory string, paths []string) []diag.Diagnostic {
	if paths == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, field,
			"%s must be omitted or contain a non-null array; an empty listing is an empty array", field)}
	}
	var diags []diag.Diagnostic
	for i, p := range paths {
		if !cleanRelativePath(p) || !atOrBelow(directory, p) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, fmt.Sprintf("%s[%d]", field, i),
				"%s[%d] must be a clean slash path relative to the workspace root at or below %q, got %q", field, i, directory, p))
		}
	}
	for i := 1; i < len(paths); i++ {
		if paths[i-1] >= paths[i] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field,
				"%s must be sorted and free of duplicates; %q precedes %q", field, paths[i-1], paths[i]))
			break
		}
	}
	return diags
}

// validateReleaseBaseline checks a release baseline: a known state, the
// pattern, and the members each state allows.
func validateReleaseBaseline(baseline *ReleaseBaseline) []diag.Diagnostic {
	if baseline == nil {
		return nil
	}
	const field = "tree.releaseBaseline"
	var diags []diag.Diagnostic
	if !oneOf(baseline.State, ReleaseBaselineStates) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+".state",
			"%s.state is %q; want one of %s", field, baseline.State, strings.Join(ReleaseBaselineStates, ", ")))
	}
	if strings.TrimSpace(baseline.Pattern) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, field+".pattern", "%s.pattern is required", field))
	}
	tagged := baseline.State == ReleaseBaselineTagged
	if tagged && baseline.Tag == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, field+".tag", "%s.tag is required when the line is tagged", field))
	}
	for _, member := range []struct {
		name string
		set  bool
	}{
		{"tag", baseline.Tag != ""},
		{"breaking", baseline.Breaking},
		{"directory", baseline.Directory != ""},
	} {
		if member.set && !tagged {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+"."+member.name,
				"%s.%s is set while the state is %q; only a tagged line carries it", field, member.name, baseline.State))
		}
	}
	if baseline.HasTags && baseline.State != ReleaseBaselineUntagged {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+".hasTags",
			"%s.hasTags is set while the state is %q; only an untagged line carries it", field, baseline.State))
	}
	if baseline.Directory != "" && !isAbsolutePath(baseline.Directory) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+".directory",
			"%s.directory must be an absolute path, got %q", field, baseline.Directory))
	}
	return diags
}

// cleanRelativePath reports whether p is a clean, non-empty slash path that
// stays inside the directory it is relative to.
func cleanRelativePath(p string) bool {
	return p != "" && p != "." && !strings.Contains(p, "\\") && !isAbsolutePath(p) &&
		path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

// atOrBelow reports whether p is directory or lies below it; "." holds every
// path.
func atOrBelow(directory, p string) bool {
	return directory == "." || p == directory || strings.HasPrefix(p, directory+"/")
}

func oneOf(value string, vocabulary []string) bool {
	for _, v := range vocabulary {
		if v == value {
			return true
		}
	}
	return false
}
