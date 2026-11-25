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
func validateTree(tree *Tree, present, tracked, ignored bool) []diag.Diagnostic {
	present = present || tree != nil
	if !present {
		return nil
	}
	if tree == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidValue, "tree",
			"tree must be omitted or contain a non-null object")}
	}
	tracked = tracked || tree.TrackedFiles != nil
	ignored = ignored || tree.IgnoredDirectories != nil
	var diags []diag.Diagnostic
	switch {
	case tree.Directory == "":
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, "tree.directory", "tree.directory is required"))
	case !cleanRelativePath(tree.Directory) && tree.Directory != ".":
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, "tree.directory",
			"tree.directory must be a clean slash path relative to the workspace root, or \".\", got %q", tree.Directory))
	}
	if tree.Repository == nil && !tracked && !ignored && tree.ReleaseBaseline == nil {
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
	return diags
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
