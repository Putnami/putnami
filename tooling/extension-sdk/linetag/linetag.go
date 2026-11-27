// Package linetag reads the last release of a version line from git.
//
// A version line renders its release tags with one pattern, such as
// `ts/v{version}`. The version bump and the checks that compare a working tree
// with the line's last release read that release here, so they agree on the
// baseline.
//
// The last release of a line is its highest release tag that HEAD reaches:
//
//   - A release tag is the pattern rendered with a MAJOR.MINOR.PATCH version
//     of decimal numbers without leading zeros. A tag that carries anything
//     else where the version goes, such as a pre-release or a word, is not a
//     release.
//   - HEAD reaches a tag when the tag's commit is HEAD or one of its
//     ancestors, through any parent.
//   - The highest tag wins, by version, whatever its distance from HEAD: an
//     older release that a merged branch carries never replaces a later one.
package linetag

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"

	"go.putnami.dev/sdk/extension/procstat"
)

// gitTimeout bounds one git call. Every call reads local objects only.
const gitTimeout = 2 * time.Minute

// Release is a release tag of a version line.
type Release struct {
	// Tag is the complete tag name.
	Tag string
	// Version is the MAJOR.MINOR.PATCH version the tag carries.
	Version string
	// Commit is the full ID of the commit the tag points at.
	Commit string
}

// Version returns the release version tag carries under the line tag pattern.
// ok is false when the tag is not a release tag of the line, or when the
// pattern does not carry exactly one version placeholder.
func Version(tag, pattern string) (version string, ok bool) {
	prefix, suffix, usable := splitPattern(pattern)
	if !usable {
		return "", false
	}
	return releaseVersion(tag, prefix, suffix)
}

// LastRelease returns the last release of the line that renders its tags with
// pattern, read from the repository that holds dir. ok is false when HEAD
// reaches no release tag of the line, which is the state of a line that never
// released. A release tag that points at no commit is not a release. A pattern
// that does not carry exactly one version placeholder, a repository without a
// commit, a release tag of the line whose object git cannot read, reached or
// not, and a missing object git reports while it walks the history from HEAD
// are errors. Git walks only as far as the tags it looks for, so a missing
// object beyond them is not read and not reported.
func LastRelease(dir, pattern string) (release Release, ok bool, err error) {
	return Open(dir).LastRelease(pattern)
}

// Tags reads the tags of the repository that holds a directory once for
// every line: HEAD, the tag names, the objects they peel to and the tags HEAD
// reaches. Each is the answer of the git call LastRelease makes for one line,
// and the same for every line, so the lines of one repository ask git four
// times between them instead of four times each. The reads describe the
// repository when the first line asks: a caller holds one Tags while HEAD and
// the tags do not move, as a run does.
type Tags struct {
	dir string

	namesOnce sync.Once
	head      string
	refs      string
	namesErr  error

	peelOnce sync.Once
	peeled   map[string]string // tag name to its cat-file --batch-check line
	peelErr  error

	mergedOnce  sync.Once
	merged      map[string]bool
	mergedLines []string // "<tag> <object>", in for-each-ref's order
	mergedErr   error
}

// Open returns the tags of the repository that holds dir, read when a line
// first asks.
func Open(dir string) *Tags { return &Tags{dir: dir} }

// LastRelease returns the last release of the line that renders its tags with
// pattern, as the package's LastRelease answers it.
func (t *Tags) LastRelease(pattern string) (release Release, ok bool, err error) {
	prefix, suffix, usable := splitPattern(pattern)
	if !usable {
		return Release{}, false, fmt.Errorf("read the last release: %q is not a usable line tag pattern", pattern)
	}
	t.namesOnce.Do(t.readNames)
	if t.namesErr != nil {
		return Release{}, false, fmt.Errorf("read the last release: %w", t.namesErr)
	}
	candidates := releases(t.refs, prefix, suffix)
	if len(candidates) == 0 {
		return Release{}, false, nil
	}
	slices.SortFunc(candidates, func(a, b Release) int { return CompareVersions(b.Version, a.Version) })

	t.peelOnce.Do(t.readPeeled)
	if t.peelErr != nil {
		return Release{}, false, fmt.Errorf("read the last release: %w", t.peelErr)
	}
	for i := range candidates {
		line := t.peeled[candidates[i].Tag]
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return Release{}, false, fmt.Errorf("read the last release: git cannot read tag %s: %s", candidates[i].Tag, line)
		}
		if fields[1] == "commit" {
			candidates[i].Commit = fields[0]
		}
	}

	t.mergedOnce.Do(t.readMerged)
	if t.mergedErr != nil {
		return Release{}, false, fmt.Errorf("read the last release: %w", t.mergedErr)
	}
	for _, candidate := range candidates {
		if candidate.Commit != "" && t.merged[candidate.Tag] {
			return candidate, true, nil
		}
	}
	return Release{}, false, nil
}

// Head returns HEAD's commit as this Tags read it: the commit every line's
// last release is reached from.
func (t *Tags) Head() (string, error) {
	t.namesOnce.Do(t.readNames)
	if t.namesErr != nil {
		return "", fmt.Errorf("read HEAD: %w", t.namesErr)
	}
	return t.head, nil
}

// HasTags reports whether the repository has a tag, of any line, as this Tags
// read the tag names.
func (t *Tags) HasTags() (bool, error) {
	t.namesOnce.Do(t.readNames)
	if t.namesErr != nil {
		return false, fmt.Errorf("read the tags: %w", t.namesErr)
	}
	return len(tagNames(t.refs)) > 0, nil
}

// TagObject is a tag and the object it peels to.
type TagObject struct {
	// Name is the tag name, without refs/tags/.
	Name string
	// Object is git's cat-file --batch-check line for the object the tag
	// peels to: "<object> <type> <size>", or "<name> missing".
	Object string
}

// Snapshot returns HEAD's commit and every tag, in for-each-ref's order, with
// the object it peels to. Together with Merged, it holds every read
// LastRelease answers a line from: a caller that holds both holds every
// answer of LastRelease, since a commit's history is named by its ID.
func (t *Tags) Snapshot() (head string, tags []TagObject, err error) {
	t.namesOnce.Do(t.readNames)
	if t.namesErr != nil {
		return "", nil, fmt.Errorf("read the tags: %w", t.namesErr)
	}
	t.peelOnce.Do(t.readPeeled)
	if t.peelErr != nil {
		return "", nil, fmt.Errorf("read the tags: %w", t.peelErr)
	}
	for _, tag := range tagNames(t.refs) {
		tags = append(tags, TagObject{Name: tag, Object: t.peeled[tag]})
	}
	return t.head, tags, nil
}

// Merged returns the tags HEAD reaches, each with the object its ref names
// ("<tag> <object>"), as the read LastRelease decides reachability from
// lists them. A caller that holds the snapshot and this listing holds every
// answer of LastRelease, even when a tag moved between the two reads.
func (t *Tags) Merged() ([]string, error) {
	t.namesOnce.Do(t.readNames)
	if t.namesErr != nil {
		return nil, fmt.Errorf("read the tags HEAD reaches: %w", t.namesErr)
	}
	t.mergedOnce.Do(t.readMerged)
	if t.mergedErr != nil {
		return nil, fmt.Errorf("read the tags HEAD reaches: %w", t.mergedErr)
	}
	return slices.Clone(t.mergedLines), nil
}

// tagNames returns the tag names of a for-each-ref listing of refs/tags.
func tagNames(refs string) []string {
	var tags []string
	for ref := range strings.SplitSeq(refs, "\n") {
		if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok && tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// readNames reads HEAD's commit and the tag names. Listing tag names reads
// no object, so every release tag of a line is a candidate, whether git can
// read its object or not.
func (t *Tags) readNames() {
	head, err := runGit(t.dir, nil, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		t.namesErr = err
		return
	}
	t.head = strings.TrimSpace(head)
	t.refs, t.namesErr = runGit(t.dir, nil, "for-each-ref", "--format=%(refname)", "refs/tags")
}

// readPeeled reads, in one cat-file --batch-check, the object each tag peels
// to: one line per tag, "<object> <type> <size>", or "<name> missing".
func (t *Tags) readPeeled() {
	tags := tagNames(t.refs)
	if len(tags) == 0 {
		t.peeled = map[string]string{}
		return
	}
	var names strings.Builder
	for _, tag := range tags {
		names.WriteString("refs/tags/" + tag + "^{}\n")
	}
	objects, err := runGit(t.dir, strings.NewReader(names.String()), "cat-file", "--batch-check")
	if err != nil {
		t.peelErr = err
		return
	}
	lines := strings.Split(strings.TrimSuffix(objects, "\n"), "\n")
	if len(lines) != len(tags) {
		t.peelErr = fmt.Errorf("git cat-file answered %d lines for %d tags", len(lines), len(tags))
		return
	}
	t.peeled = make(map[string]string, len(tags))
	for i, tag := range tags {
		t.peeled[tag] = lines[i]
	}
}

// readMerged lists every tag HEAD reaches in one walk of the history. It
// names HEAD by its commit, so a ref called HEAD cannot make it ambiguous. On
// a history it cannot read it still exits 0 and reports the missing object on
// an "error:" line of stderr. Warnings and trace lines on stderr are not
// failures.
func (t *Tags) readMerged() {
	merged, stderr, err := runGitCapture(t.dir, nil,
		"for-each-ref", "--merged="+t.head, "--format=%(refname) %(objectname)", "refs/tags")
	if err == nil {
		err = reportedError(stderr)
	}
	if err != nil {
		t.mergedErr = err
		return
	}
	t.merged = map[string]bool{}
	for line := range strings.SplitSeq(merged, "\n") {
		// A ref name holds no space, so the object name follows the first one.
		ref, object, _ := strings.Cut(line, " ")
		if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
			t.merged[tag] = true
			t.mergedLines = append(t.mergedLines, tag+" "+object)
		}
	}
}

// releases returns the release tags of the line among refs, one ref name per
// line.
func releases(refs, prefix, suffix string) []Release {
	var found []Release
	for ref := range strings.SplitSeq(refs, "\n") {
		tag := strings.TrimPrefix(ref, "refs/tags/")
		if version, isRelease := releaseVersion(tag, prefix, suffix); isRelease {
			found = append(found, Release{Tag: tag, Version: version})
		}
	}
	return found
}

// reportedError returns the "error:" and "fatal:" lines of a git stderr that
// exited 0, or nil when there is none.
func reportedError(stderr string) error {
	var reported []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(line, "error:") || strings.HasPrefix(line, "fatal:") {
			reported = append(reported, line)
		}
	}
	if len(reported) == 0 {
		return nil
	}
	return fmt.Errorf("git for-each-ref: %s", strings.Join(reported, "; "))
}

// runGit runs git in dir and returns its stdout. A failure carries git's
// stderr.
func runGit(dir string, stdin io.Reader, args ...string) (string, error) {
	stdout, _, err := runGitCapture(dir, stdin, args...)
	return stdout, err
}

// runGitCapture runs git in dir with stdin and returns its stdout and stderr.
// A failure carries git's stderr.
func runGitCapture(dir string, stdin io.Reader, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := procstat.Run(cmd, procstat.Git); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", stderr.String(), fmt.Errorf("git %s (in %s): timed out after %s", args[0], dir, gitTimeout)
		}
		return "", stderr.String(), fmt.Errorf("git %s (in %s): %w: %s", args[0], dir, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}

// splitPattern returns what a line tag pattern holds before and after its
// version placeholder. usable is false for a pattern that does not carry
// exactly one placeholder, holds a space or a glob character, or starts with a
// dash.
func splitPattern(pattern string) (prefix, suffix string, usable bool) {
	placeholder := wsproto.LineTagPlaceholder
	if strings.Count(pattern, placeholder) != 1 || strings.ContainsAny(pattern, " \t\n*?[") || strings.HasPrefix(pattern, "-") {
		return "", "", false
	}
	prefix, suffix, _ = strings.Cut(pattern, placeholder)
	return prefix, suffix, true
}

// releaseVersion returns the version tag carries between prefix and suffix,
// when it is a release version.
func releaseVersion(tag, prefix, suffix string) (string, bool) {
	if len(tag) <= len(prefix)+len(suffix) || !strings.HasPrefix(tag, prefix) || !strings.HasSuffix(tag, suffix) {
		return "", false
	}
	version := tag[len(prefix) : len(tag)-len(suffix)]
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') || strings.Trim(part, "0123456789") != "" {
			return "", false
		}
	}
	return version, true
}

// CompareVersions orders two MAJOR.MINOR.PATCH release versions, such as the
// ones Version returns: negative when a is the lower, positive when a is the
// higher. Numbers compare by length, then by digits, so no number overflows.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range as {
		if len(as[i]) != len(bs[i]) {
			return len(as[i]) - len(bs[i])
		}
		if as[i] != bs[i] {
			return strings.Compare(as[i], bs[i])
		}
	}
	return 0
}
