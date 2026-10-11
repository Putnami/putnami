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
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
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
	prefix, suffix, usable := splitPattern(pattern)
	if !usable {
		return Release{}, false, fmt.Errorf("read the last release: %q is not a usable line tag pattern", pattern)
	}
	head, err := runGit(dir, nil, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return Release{}, false, fmt.Errorf("read the last release: %w", err)
	}
	head = strings.TrimSpace(head)
	// Listing tag names reads no object, so every release tag of the line is a
	// candidate here, whether git can read its object or not.
	refs, err := runGit(dir, nil, "for-each-ref", "--format=%(refname)", "refs/tags")
	if err != nil {
		return Release{}, false, fmt.Errorf("read the last release: %w", err)
	}
	candidates := releases(refs, prefix, suffix)
	if len(candidates) == 0 {
		return Release{}, false, nil
	}
	slices.SortFunc(candidates, func(a, b Release) int { return compareVersions(b.Version, a.Version) })

	// One line per candidate, in order: "<object> <type> <size>" for the
	// object the tag peels to, or "<name> missing".
	var names strings.Builder
	for _, candidate := range candidates {
		names.WriteString("refs/tags/" + candidate.Tag + "^{}\n")
	}
	objects, err := runGit(dir, strings.NewReader(names.String()), "cat-file", "--batch-check")
	if err != nil {
		return Release{}, false, fmt.Errorf("read the last release: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(objects, "\n"), "\n")
	if len(lines) != len(candidates) {
		return Release{}, false, fmt.Errorf("read the last release: git cat-file answered %d lines for %d tags", len(lines), len(candidates))
	}
	for i := range candidates {
		fields := strings.Fields(lines[i])
		if len(fields) != 3 {
			return Release{}, false, fmt.Errorf("read the last release: git cannot read tag %s: %s", candidates[i].Tag, lines[i])
		}
		if fields[1] == "commit" {
			candidates[i].Commit = fields[0]
		}
	}

	// --merged lists every tag HEAD reaches in one walk of the history. It
	// names HEAD by its commit, so a ref called HEAD cannot make it ambiguous.
	// On a history it cannot read it still exits 0 and reports the missing
	// object on an "error:" line of stderr. Warnings and trace lines on stderr
	// are not failures.
	merged, stderr, err := runGitCapture(dir, nil,
		"for-each-ref", "--merged="+head, "--format=%(refname)", "refs/tags")
	if err == nil {
		err = reportedError(stderr)
	}
	if err != nil {
		return Release{}, false, fmt.Errorf("read the last release: %w", err)
	}
	reached := map[string]bool{}
	for _, release := range releases(merged, prefix, suffix) {
		reached[release.Tag] = true
	}
	for _, candidate := range candidates {
		if candidate.Commit != "" && reached[candidate.Tag] {
			return candidate, true, nil
		}
	}
	return Release{}, false, nil
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
	if err := cmd.Run(); err != nil {
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

// compareVersions orders two release versions: negative when a is the lower,
// positive when a is the higher. Numbers compare by length, then by digits, so
// no number overflows.
func compareVersions(a, b string) int {
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
