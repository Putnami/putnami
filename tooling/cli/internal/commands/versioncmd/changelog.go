package versioncmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/tooling/cli/internal/git"
)

// changelogDateLayout is the date of a release heading: a plain ISO day, no
// time and no zone, because a release is dated by the day it was cut.
const changelogDateLayout = "2006-01-02"

// changelogSections are the changelog's sections, in rendering order, with the
// conventional commit types each one collects. Breaking changes are their own
// section rather than a marker inside another, because the reader looking for
// them is doing a different job from the one reading the feature list.
//
// A commit whose type is not listed lands in "Other". A commit that is not a
// conventional commit at all is dropped: it says nothing a release note can
// carry, and inventing a bullet for it would put merge commits in the notes.
var changelogSections = []struct {
	title string
	types []string
}{
	{title: "Breaking"},
	{title: "Features", types: []string{"feat"}},
	{title: "Fixes", types: []string{"fix"}},
	{title: "Performance", types: []string{"perf"}},
	{title: "Other"},
}

// RenderChangelog renders one release's notes from the commits it contains.
//
// The rendering is deterministic — same commits, same bytes — because it is
// written three times for one release: into the annotated tag message, into the
// GitHub release, and into the line's CHANGELOG.md. Three copies that could
// differ would make the file the only one anybody trusts.
//
// Commits arrive newest-first, as git reports them, and stay in that order
// inside their section.
func RenderChangelog(line, version string, commits []git.Commit) string {
	return renderChangelogAt(line, version, commits, time.Now())
}

// renderChangelogAt is RenderChangelog with the release date injected, so the
// golden test pins the rendering rather than today.
func renderChangelogAt(line, version string, commits []git.Commit, at time.Time) string {
	grouped := groupChangelogCommits(commits)
	var out strings.Builder
	fmt.Fprintf(&out, "## %s — %s\n", version, at.UTC().Format(changelogDateLayout))
	for _, section := range changelogSections {
		bullets := grouped[section.title]
		if len(bullets) == 0 {
			continue
		}
		fmt.Fprintf(&out, "\n### %s\n\n", section.title)
		for _, bullet := range bullets {
			out.WriteString(bullet)
			out.WriteString("\n")
		}
	}
	if out.Len() == len(fmt.Sprintf("## %s — %s\n", version, at.UTC().Format(changelogDateLayout))) {
		out.WriteString("\nNo conventional commits since the previous release of " + line + ".\n")
	}
	return out.String()
}

// groupChangelogCommits sorts each commit into its section's bullet list.
func groupChangelogCommits(commits []git.Commit) map[string][]string {
	grouped := make(map[string][]string, len(changelogSections))
	for _, commit := range commits {
		typ, breaking, ok := git.ParseConventional(commit.Subject, commit.Body)
		if !ok {
			continue
		}
		grouped[changelogSectionFor(typ, breaking)] = append(
			grouped[changelogSectionFor(typ, breaking)], changelogBullet(commit))
	}
	return grouped
}

// changelogSectionFor names the section a commit belongs to. A breaking change
// is reported as breaking whatever its type, because that is what the reader
// needs first.
func changelogSectionFor(typ string, breaking bool) string {
	if breaking {
		return "Breaking"
	}
	for _, section := range changelogSections {
		for _, candidate := range section.types {
			if candidate == typ {
				return section.title
			}
		}
	}
	return "Other"
}

// changelogBullet renders one commit: its subject and the short SHA that
// identifies it, which is what a reader follows back to the change.
func changelogBullet(commit git.Commit) string {
	sha := commit.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return fmt.Sprintf("- %s (%s)", commit.Subject, sha)
}

// PrependChangelog writes notes at the top of the changelog at path, above
// whatever is already there, and creates the file when it does not exist.
//
// Prepending rather than rewriting is what keeps the file a history: a release
// adds its own entry and never restates the ones before it, so an entry a human
// edited after the fact stays edited.
func PrependChangelog(path, notes string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	body := strings.TrimRight(notes, "\n") + "\n"
	if len(strings.TrimSpace(string(existing))) > 0 {
		body += "\n" + strings.TrimLeft(string(existing), "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create the changelog directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
