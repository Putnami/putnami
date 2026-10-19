package versioncmd

import (
	"fmt"
	"strings"

	"go.putnami.dev/tooling/cli/internal/git"
)

// LineReport is one version line as `version get` prints it and `version tag`
// releases it, computed by the functions those commands call. The host RPC
// method version.lines answers with it.
type LineReport struct {
	// Spec is the line: its scope path and tag pattern.
	Spec git.LineSpec
	// Info is the line's version at HEAD (git.GetVersionInfo).
	Info *git.VersionInfo
	// Release is the release `version tag` creates without an explicit tag.
	// It is nil when HEAD already carries the line's release tag, which
	// `version tag` refuses to release again.
	Release *ProposedRelease
	// Commits are the commits the release's changelog renders (lineCommits),
	// newest first.
	Commits []git.Commit
}

// ProposedRelease is the version and tag `version tag` proposes for a line.
type ProposedRelease struct {
	Version string
	Tag     string
}

// LineSpecs are the workspace's version lines, sorted by scope path, as
// `version get` and `version tag` read them.
func LineSpecs(wsRoot string) ([]git.LineSpec, error) {
	return lineSpecs(wsRoot)
}

// SelectLines picks the lines a --scope value names, as `version get` does:
// every line when the scope is empty once its leading and trailing slashes are
// trimmed, else the one line whose scope path it is. A scope that names no line
// is the ErrNotFound error that lists the declared lines.
func SelectLines(specs []git.LineSpec, scope string) ([]git.LineSpec, error) {
	if strings.Trim(scope, "/") == "" {
		return specs, nil
	}
	spec, err := selectLine(specs, scope)
	if err != nil {
		return nil, err
	}
	return []git.LineSpec{spec}, nil
}

// ReportLine computes one line in the order `version tag` does: its version at
// HEAD, then, unless HEAD is a release of the line, the release it proposes,
// then the commits since the line's last release.
func ReportLine(wsRoot string, spec git.LineSpec) (LineReport, error) {
	info, err := git.GetVersionInfo(wsRoot, spec)
	if err != nil {
		return LineReport{}, err
	}
	report := LineReport{Spec: spec, Info: info}
	if !info.Tagged {
		version, tag, err := proposedTag(wsRoot, spec, "")
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

// BoundRevision is the full commit ID requireSourceRevisionAtHead compares
// with HEAD: the commit PUTNAMI_SOURCE_REVISION binds the run to when it is
// set, else HEAD's, in lower case.
func BoundRevision(wsRoot string) (string, error) {
	revision, set, err := git.SourceRevisionOverride()
	if err != nil {
		return "", err
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
