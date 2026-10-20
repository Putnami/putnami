package versioncmd

import (
	"fmt"
	"strings"

	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/git"
)

// LineReport is one version line with its version and its next release. The
// host RPC method version.lines answers with it, and the version commands of
// @putnami/dev print or release from it.
type LineReport struct {
	// Spec is the line: its scope path and tag pattern.
	Spec git.LineSpec
	// Info is the line's version at HEAD (git.GetVersionInfo).
	Info *git.VersionInfo
	// Release is the line's next release when no explicit tag names it. It
	// is nil when HEAD already carries the line's release tag, which is never
	// released again.
	Release *ProposedRelease
	// Commits are the commits the release's changelog renders (lineCommits),
	// newest first.
	Commits []git.Commit
}

// ProposedRelease is the version and tag a line's next release takes.
type ProposedRelease struct {
	Version string
	Tag     string
}

// LineSpecs are the workspace's version lines, sorted by scope path.
func LineSpecs(wsRoot string) ([]git.LineSpec, error) {
	return lineSpecs(wsRoot)
}

// SelectLines picks the lines a --scope value names: every line when the scope
// is empty once its leading and trailing slashes are trimmed, else the one line
// whose scope path it is. A scope that names no line is the ErrNotFound error
// that lists the declared lines by the trimmed scope.
func SelectLines(specs []git.LineSpec, scope string) ([]git.LineSpec, error) {
	scope = strings.Trim(scope, "/")
	if scope == "" {
		return specs, nil
	}
	spec, err := selectLine(specs, scope)
	if err != nil {
		return nil, err
	}
	return []git.LineSpec{spec}, nil
}

// ReportLine computes one line in this order: its version at HEAD, then,
// unless HEAD is a release of the line, the release it proposes, then the
// commits since the line's last release.
func ReportLine(wsRoot string, spec git.LineSpec) (LineReport, error) {
	info, err := git.GetVersionInfo(wsRoot, spec)
	if err != nil {
		return LineReport{}, err
	}
	report := LineReport{Spec: spec, Info: info}
	if !info.Tagged {
		version, tag, err := proposedTag(wsRoot, spec)
		if err != nil {
			return LineReport{}, err
		}
		report.Release = &ProposedRelease{Version: version, Tag: tag}
	}
	if report.Commits, err = lineCommits(wsRoot, spec); err != nil {
		return LineReport{}, err
	}
	return report, nil
}

// BoundRevision is the full commit ID a run is bound to, which a release
// compares with HEAD: the commit PUTNAMI_SOURCE_REVISION names when it is set,
// else HEAD's, in lower case. A malformed PUTNAMI_SOURCE_REVISION is an
// ErrInvalidConfig error.
func BoundRevision(wsRoot string) (string, error) {
	revision, set, err := git.SourceRevisionOverride()
	if err != nil {
		return "", cmderr.InvalidConfigf("version: %v", err)
	}
	if set {
		return revision, nil
	}
	head, err := git.HeadSHA(wsRoot)
	if err != nil {
		return "", fmt.Errorf("read HEAD: %w", err)
	}
	return strings.ToLower(head), nil
}
