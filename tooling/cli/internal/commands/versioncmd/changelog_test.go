package versioncmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/git"
)

// changelogFixtureCommits is one release's range, newest first, as git reports
// it: every section, plus the two spellings of a breaking change and two
// commits a release note must not invent a bullet for.
func changelogFixtureCommits() []git.Commit {
	return []git.Commit{
		{SHA: "1111111aaaaaaa", Subject: "feat(web): add the thing"},
		{SHA: "2222222bbbbbbb", Subject: "feat(cli)!: rename the flag"},
		{SHA: "3333333ccccccc", Subject: "refactor(core): move it", Body: "why\n\nBREAKING CHANGE: the old path is gone"},
		{SHA: "4444444ddddddd", Subject: "fix(web): stop the leak"},
		{SHA: "5555555eeeeeee", Subject: "perf(cli): halve the walk"},
		{SHA: "6666666fffffff", Subject: "docs: rewrite the guide"},
		{SHA: "7777777aaaaaaa", Subject: "Merge pull request #1 from a/b"},
		{SHA: "8888888bbbbbbb", Subject: "an ordinary message"},
	}
}

// The rendering is written three times for one release — tag message, GitHub
// release, CHANGELOG.md — so it is pinned byte for byte.
func TestRenderChangelogMatchesTheGolden(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 4, 11, 30, 0, 0, time.UTC)
	got := renderChangelogAt("typescript", "0.5.0", changelogFixtureCommits(), at)

	goldenPath := filepath.Join("testdata", "changelog.golden")
	if os.Getenv("UPDATE_CHANGELOG_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("rendered changelog:\n%s\nwant:\n%s", got, want)
	}
}

// A range with nothing to report still renders a heading: a release happened,
// and a changelog that said nothing at all would read as a missing entry.
func TestRenderChangelogWithNoConventionalCommits(t *testing.T) {
	t.Parallel()
	got := renderChangelogAt("tooling", "0.0.1",
		[]git.Commit{{SHA: "1111111", Subject: "an ordinary message"}}, time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(got, "## 0.0.1 — 2026-09-04\n") {
		t.Fatalf("rendered = %q, want the dated heading", got)
	}
	if !strings.Contains(got, "No conventional commits since the previous release of tooling.") {
		t.Fatalf("rendered = %q, want the empty-range sentence", got)
	}
}

// The changelog is a history: a release adds its entry above the previous ones
// and never restates them, so an entry a human edited stays edited.
func TestPrependChangelogKeepsTheExistingHistory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "typescript", "CHANGELOG.md")

	if err := PrependChangelog(path, "## 0.4.0 — 2026-08-01\n\n### Fixes\n\n- fix: a (1111111)\n"); err != nil {
		t.Fatal(err)
	}
	if err := PrependChangelog(path, "## 0.5.0 — 2026-09-04\n\n### Features\n\n- feat: b (2222222)\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	newest, older := strings.Index(body, "## 0.5.0"), strings.Index(body, "## 0.4.0")
	if newest != 0 || older <= newest {
		t.Fatalf("changelog = %q, want the newest release first", body)
	}
}
