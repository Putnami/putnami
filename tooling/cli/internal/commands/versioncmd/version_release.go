package versioncmd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/linetag"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// proposedTag computes the version the line's next release takes and the tag
// that names it.
func proposedTag(wsRoot string, spec git.LineSpec) (version, tagName string, err error) {
	release, found, err := lastRelease(wsRoot, spec)
	if err != nil {
		return "", "", err
	}
	last := "0.0.0"
	bump := git.BumpNone
	if found {
		last = release.Version
		commits, commitsErr := git.CommitsSince(wsRoot, release.Commit, spec.Pathspecs)
		if commitsErr != nil {
			return "", "", commitsErr
		}
		bump, err = git.BumpFor(commits, majorOf(last) == 0, git.LineStableTest(wsRoot, spec))
		if err != nil {
			return "", "", err
		}
	}
	version = git.NextVersion(last, bump, false)
	return version, wsproto.RenderLineTag(spec.TagPattern, version), nil
}

// lineCommits are the commits the changelog renders: everything since the
// line's last tag that touches it, or its whole history when it has none.
func lineCommits(wsRoot string, spec git.LineSpec) ([]git.Commit, error) {
	release, _, err := lastRelease(wsRoot, spec)
	if err != nil {
		return nil, err
	}
	return git.CommitsSince(wsRoot, release.Commit, spec.Pathspecs)
}

// lastRelease is the line's last release (linetag.LastRelease).
func lastRelease(wsRoot string, spec git.LineSpec) (linetag.Release, bool, error) {
	if strings.Count(spec.TagPattern, wsproto.LineTagPlaceholder) != 1 {
		return linetag.Release{}, false, cmderr.InvalidConfigf(
			"line %s has tag pattern %q, which carries no %s placeholder",
			printableLine(spec.ScopePath), spec.TagPattern, wsproto.LineTagPlaceholder)
	}
	return linetag.LastRelease(wsRoot, spec.TagPattern)
}

// lineSpecs are the workspace's version lines, in sorted scope-path order, each
// bound to the paths whose commits advance it and to the support catalog that
// caps the bump of a commit touching no stable project.
func lineSpecs(wsRoot string) ([]git.LineSpec, error) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	paths := make([]string, 0, len(ws.Lines))
	for path := range ws.Lines {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	// A release must not read an unusable catalog as absent: that would raise
	// the bump of every preview project without a word.
	stable, err := workspace.StableChangeTest(ws)
	if err != nil {
		return nil, cmderr.InvalidConfigf("version: %v", err)
	}
	specs := make([]git.LineSpec, 0, len(paths))
	for _, path := range paths {
		spec := git.LineSpec{ScopePath: path, TagPattern: ws.Lines[path], Stable: stable}
		if path != "" {
			spec.Pathspecs = []string{path}
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		specs = append(specs, git.LineSpec{TagPattern: wsproto.LineTagPattern("", nil), Stable: stable})
	}
	return specs, nil
}

// selectLine resolves a --scope value to one declared line.
func selectLine(specs []git.LineSpec, scope string) (git.LineSpec, error) {
	wanted := strings.Trim(scope, "/")
	for _, spec := range specs {
		if spec.ScopePath == wanted {
			return spec, nil
		}
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, printableLine(spec.ScopePath))
	}
	return git.LineSpec{}, cmderr.NotFoundf("no version line %q; this workspace declares %s",
		scope, strings.Join(names, ", "))
}

// printableLine names a line for a human: the root line has no path, so it is
// spelled with the workspace's own marker rather than an empty column.
func printableLine(scopePath string) string {
	if scopePath == "" {
		return "(workspace root)"
	}
	return scopePath
}

// majorOf reads the major number of a version, or 0 when it has none.
func majorOf(version string) int {
	major := 0
	if _, err := fmt.Sscanf(strings.TrimPrefix(version, "v"), "%d", &major); err != nil {
		return 0
	}
	return major
}

func gitOutput(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
