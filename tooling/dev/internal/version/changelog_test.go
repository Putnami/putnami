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

// A subject keeps its words and loses the references to a tracker. The gate of
// a public cut refuses a bare one.
func TestChangelogBulletCarriesNoTrackerReference(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "changelog-needs-no-edit", "the-changelog-carries-no-tracker-reference")
	// @ is the # of a reference, and ~ its absence: the public cut scans this file.
	written := strings.NewReplacer("@", "#", "~", "")
	for template, want := range map[string]string{
		"fix(cli): stop the leak (@123)":                   "fix(cli): stop the leak",
		"fix(cli): stop the leak (@5) (@12)":               "fix(cli): stop the leak",
		"feat: a thing (epic @895) (@919)":                 "feat: a thing",
		"feat: a thing (@12, @13)":                         "feat: a thing",
		"feat: a thing [@12 and @13]":                      "feat: a thing",
		"feat: a thing (org/repo@12)":                      "feat: a thing",
		"docs: the notes after the issue @951 review":      "docs: the notes after the review",
		"docs: the notes after PR ~951 and issue ~952":     "docs: the notes after",
		"docs: the decisions, @256, @257: kept":            "docs: the decisions: kept",
		"fix: a page renders without error @418 (@20)":     "fix: a page renders without error",
		"fix: @12 breaks the build":                        "fix: breaks the build",
		"fix: a thing (@12 and the rest)":                  "fix: a thing (and the rest)",
		"fix: a thing (see @12)":                           "fix: a thing (see)",
		"fix: row @1 of the table":                         "fix: row @1 of the table",
		"fix: read PKCS@1 keys in C@ 8":                    "fix: read PKCS@1 keys in C@ 8",
		"docs: link to guide.md@10-heading, (@anchor)":     "docs: link to guide.md@10-heading, (@anchor)",
		"fix: keep  two spaces and ( odd ) brackets (@12)": "fix: keep  two spaces and ( odd ) brackets",
	} {
		subject, want := written.Replace(template), written.Replace(want)
		if got := withoutTrackerReferences(subject); got != want {
			t.Errorf("withoutTrackerReferences(%q) = %q, want %q", subject, got, want)
		}
		if got := changelogBullet(wire.LineCommit{SHA: "1111111aaaaaaa", Subject: subject}); got != "- "+want+" (1111111)" {
			t.Errorf("bullet = %q, want %q without a reference", got, want)
		}
	}
}

// A release goes under the title and its introduction, above the releases
// before it, whose bytes stay.
func TestPrependChangelogWritesUnderTheTitle(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "changelog-needs-no-edit", "a-release-goes-under-the-title")
	const entry, older = "## 0.5.0 — 2026-09-04\n\n- feat: b (2222222)\n", "## 0.4.0 — 2026-08-01\n\n\n- fix: a (1111111)\n\n"
	for name, test := range map[string]struct{ existing, want string }{
		"title":                {"# Changelog — go\n\n" + older, "# Changelog — go\n\n" + entry + "\n" + older},
		"title and intro":      {"\n# Changelog\n\nEvery release.\n" + older, "# Changelog\n\nEvery release.\n\n" + entry + "\n" + older},
		"title only":           {"# Changelog", "# Changelog\n\n" + entry},
		"no title":             {older, entry + "\n" + older},
		"text without heading": {"Notes.\n", entry + "\nNotes.\n"},
	} {
		path := filepath.Join(t.TempDir(), "CHANGELOG.md")
		if err := os.WriteFile(path, []byte(test.existing), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := PrependChangelog(path, "# Changelog — other", entry+"\n"); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != test.want {
			t.Errorf("%s: changelog = %q (%v), want %q", name, got, err, test.want)
		}
	}
}

// A missing or empty changelog is created with its title, and the releases
// after it go under that title.
func TestPrependChangelogCreatesTheFileWithItsTitle(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "changelog-needs-no-edit", "a-missing-changelog-gets-its-title")
	dir := t.TempDir()
	missing := filepath.Join(dir, "typescript", "CHANGELOG.md")
	empty := filepath.Join(dir, "CHANGELOG.md")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, title := range map[string]string{missing: changelogTitle("typescript"), empty: changelogTitle("")} {
		for _, notes := range []string{"## 0.4.0\n\n- fix: a (1111111)\n", "## 0.5.0\n\n- feat: b (2222222)\n\n"} {
			if err := PrependChangelog(path, title, notes); err != nil {
				t.Fatal(err)
			}
		}
		want := title + "\n\n## 0.5.0\n\n- feat: b (2222222)\n\n## 0.4.0\n\n- fix: a (1111111)\n"
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("changelog = %q (%v), want %q", got, err, want)
		}
	}
	if root, line := changelogTitle(""), changelogTitle("typescript"); root != "# Changelog" || line != "# Changelog — typescript" {
		t.Errorf("titles = %q and %q", root, line)
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
	if err := PrependChangelog(filepath.Join(blocker, "CHANGELOG.md"), "# Changelog", "## 0.1.0\n"); err == nil || !strings.Contains(err.Error(), "CHANGELOG.md") && !strings.Contains(err.Error(), "changelog") {
		t.Fatalf("err = %v, want the changelog named", err)
	}
	if err := PrependChangelog(dir, "# Changelog", "## 0.1.0\n"); err == nil || !strings.HasPrefix(err.Error(), "read ") {
		t.Fatalf("err = %v, want the read error of a directory", err)
	}
}
