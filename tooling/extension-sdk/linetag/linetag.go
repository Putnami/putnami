// Package linetag reads the last release of a version line from git.
//
// A version line renders its release tags with one pattern, such as
// `ts/v{version}`. The version bump and the checks that compare a working tree
// with the line's last release read that release here, so they agree on the
// baseline.
//
// The last release of a line is its highest release tag on the first-parent
// history of HEAD:
//
//   - A release tag is the pattern rendered with a MAJOR.MINOR.PATCH version
//     of decimal numbers without leading zeros. A tag that carries anything
//     else where the version goes, such as a pre-release or a word, is not a
//     release.
//   - The first-parent history is HEAD and the chain of first parents behind
//     it. A tag on a commit that only a merged branch holds is not on it.
//   - The highest tag wins, by version, whatever its distance from HEAD.
package linetag

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
)

// gitTimeout bounds the git call, which reads local objects only.
const gitTimeout = 2 * time.Minute

// tagDecoration opens the name of a tag in a `--decorate=full` decoration.
const tagDecoration = "tag: refs/tags/"

// Release is a release tag of a version line.
type Release struct {
	// Tag is the complete tag name.
	Tag string
	// Version is the MAJOR.MINOR.PATCH version the tag carries.
	Version string
	// Commit is the full ID of the commit the tag points at.
	Commit string
}

// LastRelease returns the last release of the line that renders its tags with
// pattern, read from the repository that holds dir. ok is false when the
// first-parent history of HEAD holds no release tag of the line, which is the
// state of a line that never released. A pattern that does not carry exactly
// one version placeholder, a repository without a commit and a history git
// cannot read are errors.
func LastRelease(dir, pattern string) (release Release, ok bool, err error) {
	placeholder := wsproto.LineTagPlaceholder
	if strings.Count(pattern, placeholder) != 1 || strings.ContainsAny(pattern, " \t\n*?[") || strings.HasPrefix(pattern, "-") {
		return Release{}, false, fmt.Errorf("read the last release: %q is not a usable line tag pattern", pattern)
	}
	prefix, suffix, _ := strings.Cut(pattern, placeholder)

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	// One record per first-parent commit: its ID, NUL, then the tags that
	// point at it. --decorate-refs takes precedence over log.excludeDecoration.
	cmd := exec.CommandContext(ctx, "git", "log", "--first-parent", "--no-show-signature",
		"--decorate=full", "--decorate-refs=refs/tags", "--format=%H%x00%D", "HEAD")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if runErr := cmd.Run(); runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Release{}, false, fmt.Errorf("read the last release: git log (in %s): timed out after %s", dir, gitTimeout)
		}
		return Release{}, false, fmt.Errorf("read the last release: git log (in %s): %w: %s", dir, runErr, strings.TrimSpace(stderr.String()))
	}

	for record := range strings.SplitSeq(stdout.String(), "\n") {
		commit, decorations, found := strings.Cut(record, "\x00")
		if !found || decorations == "" {
			continue
		}
		// A ref name holds no space, so ", " separates two decorations.
		for decoration := range strings.SplitSeq(decorations, ", ") {
			tag, isTag := strings.CutPrefix(decoration, tagDecoration)
			if !isTag {
				continue
			}
			version, isRelease := releaseVersion(tag, prefix, suffix)
			if !isRelease || (ok && !higher(version, release.Version)) {
				continue
			}
			release, ok = Release{Tag: tag, Version: version, Commit: commit}, true
		}
	}
	return release, ok, nil
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

// higher reports whether release version a orders after release version b.
// Numbers compare by length, then by digits, so no number overflows.
func higher(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range as {
		if len(as[i]) != len(bs[i]) {
			return len(as[i]) > len(bs[i])
		}
		if as[i] != bs[i] {
			return as[i] > bs[i]
		}
	}
	return false
}
