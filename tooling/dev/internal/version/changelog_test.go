package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// changelogFixtureCommits is one release's range, newest first, as the host
// lists it: every section, plus the two spellings of a breaking change and
// two commits a release note must not invent a bullet for.
func changelogFixtureCommits() []wire.LineCommit {
	return []wire.LineCommit{
		{SHA: "1111111aaaaaaa", Subject: "feat(web): add the thing"},
		{SHA: "2222222bbbbbbb", Subject: "feat(cli)!: rename the flag"},
		{SHA: "3333333ccccccc", Subject: "refactor(core): move it", Body: "why\n\nBREAKING CHANGE: the old path is gone"},
		{SHA: "4444444ddddddd", Subject: "fix(web): stop the leak"},
		{SHA: "5555555eeeeeee", Subject: "perf(cli): halve the walk"},
		{SHA: "6666666fffffff", Subject: "docs: rewrite the guide"},
		{SHA: "7777777aaaaaaa", Subject: "Merge branch 'a' into b"},
		{SHA: "8888888bbbbbbb", Subject: "an ordinary message"},
	}
}

// The rendering is written twice for one release — tag message and
// CHANGELOG.md — so it is pinned byte for byte.
func TestRenderChangelogMatchesTheGolden(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "release-commit-then-tag", "the-changelog-matches-the-golden")
	at := time.Date(2026, 9, 4, 11, 30, 0, 0, time.UTC)
	got := renderChangelogAt("typescript", "0.5.0", changelogFixtureCommits(), at)

	want, err := os.ReadFile(filepath.Join("testdata", "changelog.golden"))
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
		[]wire.LineCommit{{SHA: "1111111", Subject: "an ordinary message"}}, time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC))
	if want := "## 0.0.1 — 2026-09-04\n\nNo conventional commits since the previous release of tooling.\n"; got != want {
		t.Fatalf("rendered = %q, want %q", got, want)
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
	if err := PrependChangelog(path, "## 0.5.0 — 2026-09-04\n\n### Features\n\n- feat: b (2222222)\n\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "## 0.5.0 — 2026-09-04\n\n### Features\n\n- feat: b (2222222)\n\n## 0.4.0 — 2026-08-01\n\n### Fixes\n\n- fix: a (1111111)\n"
	if body := string(data); body != want {
		t.Fatalf("changelog = %q, want %q", body, want)
	}
}

// A changelog path the command cannot write is an error, never a silent
// release without notes.
func TestPrependChangelogReportsAPathItCannotWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "tooling")
	if err := os.WriteFile(blocker, []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrependChangelog(filepath.Join(blocker, "CHANGELOG.md"), "## 0.1.0\n"); err == nil || !strings.Contains(err.Error(), "CHANGELOG.md") && !strings.Contains(err.Error(), "changelog") {
		t.Fatalf("err = %v, want the changelog named", err)
	}
	if err := PrependChangelog(dir, "## 0.1.0\n"); err == nil || !strings.HasPrefix(err.Error(), "read ") {
		t.Fatalf("err = %v, want the read error of a directory", err)
	}
}
