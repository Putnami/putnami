package versioncmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// branchWithCommits is a repository whose branch "change" holds one commit per
// message on top of main. It returns the directory and the commit IDs, oldest
// first.
func branchWithCommits(t *testing.T, messages ...string) (string, []string) {
	t.Helper()
	dir := versionLineRepo(t)
	gitInRepo(t, dir, "checkout", "-b", "change")
	shas := make([]string, 0, len(messages))
	for i, message := range messages {
		writeRepoFile(t, dir, "tooling/cli/src.go", "package cli\n\nconst step = "+string(rune('0'+i))+"\n")
		gitInRepo(t, dir, "add", "-A")
		gitInRepo(t, dir, "commit", "-m", message)
		shas = append(shas, gitOutputInRepo(t, dir, "rev-parse", "HEAD"))
	}
	return dir, shas
}

func runVersionCheck(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	return sharedtest.CaptureStdout(t, func() error { return VersionCheck(dir, args) })
}

// The squash commit keeps the title and drops the branch commits, so a marker
// that only a branch commit carries is lost: the check fails and names it.
func TestVersionCheckFailsATitleThatDropsTheBreakingMarkerAndNamesTheCommit(t *testing.T) {
	spectest.Proves(t, "cli/version-from-git", "squash-keeps-the-breaking-marker", "dropped-marker-fails-and-names-the-commit")
	dir, shas := branchWithCommits(t, "fix(cli): stop the leak", "feat(cli)!: drop the --all flag")

	_, err := runVersionCheck(t, dir, "--base", "main", "--title", "feat(cli): rework the selection")
	if err == nil {
		t.Fatal("VersionCheck passed a title without the marker a branch commit declares")
	}
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitFailure {
		t.Errorf("exit code = %d, want the failure code %d: a dropped marker is not a usage error", code, protocolcli.ExitFailure)
	}
	message := err.Error()
	if want := shas[1][:12] + " feat(cli)!: drop the --all flag"; !strings.Contains(message, want) {
		t.Errorf("error = %q, want it to name the breaking commit %q", message, want)
	}
	if strings.Contains(message, shas[0][:12]) {
		t.Errorf("error = %q, names the commit %s that declares nothing", message, shas[0][:12])
	}
	if !strings.Contains(message, `Add "!"`) {
		t.Errorf("error = %q, want it to say how to declare the break", message)
	}
}

func TestVersionCheckPassesATitleThatCarriesTheMarker(t *testing.T) {
	spectest.Proves(t, "cli/version-from-git", "squash-keeps-the-breaking-marker", "title-with-the-marker-passes")
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")

	out, err := runVersionCheck(t, dir, "--base", "main", "--title", "feat(cli)!: rework the selection (#12)")
	if err != nil {
		t.Fatalf("VersionCheck: %v", err)
	}
	if !strings.Contains(out, "1 commit") {
		t.Errorf("output = %q, want it to count the breaking commit", out)
	}
}

// A title with the marker passes whatever the branch says, and so does any
// title when no branch commit declares a break.
func TestVersionCheckPassesAnyTitleWithoutABreakingCommit(t *testing.T) {
	spectest.Proves(t, "cli/version-from-git", "squash-keeps-the-breaking-marker", "no-breaking-commit-passes-any-title")
	dir, _ := branchWithCommits(t, "feat(cli): add a flag", "fix(cli): stop the leak")

	for _, title := range []string{"feat(cli): add a flag", "feat(cli)!: add a flag", "Add a flag", "WIP"} {
		if _, err := runVersionCheck(t, dir, "--base", "main", "--title", title); err != nil {
			t.Errorf("VersionCheck with title %q: %v", title, err)
		}
	}
}

// A footer in a branch commit body declares a break as "!" does.
func TestVersionCheckReadsTheBreakingFooterOfABranchCommit(t *testing.T) {
	dir, shas := branchWithCommits(t, "refactor(cli): drop the greeting\n\nBREAKING CHANGE: Greet is gone; use Wave.")

	_, err := runVersionCheck(t, dir, "--base", "main", "--title", "refactor(cli): drop the greeting")
	if err == nil || !strings.Contains(err.Error(), shas[0][:12]) {
		t.Fatalf("error = %v, want a failure that names %s", err, shas[0][:12])
	}
}

// The body counts only when the caller passes it: a host that squashes with
// the pull request body keeps its footer in the commit `putnami version` reads.
func TestVersionCheckCountsABreakingFooterInTheBodyFile(t *testing.T) {
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")
	body := filepath.Join(t.TempDir(), "body.md")
	// A host returns a body with CRLF line ends.
	if err := os.WriteFile(body, []byte("Drop the flag.\r\n\r\nBREAKING CHANGE: --all is gone.\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runVersionCheck(t, dir, "--base", "main", "--title", "feat(cli): rework the selection", "--body-file", body); err != nil {
		t.Fatalf("VersionCheck with a breaking footer in the body: %v", err)
	}
	if _, err := runVersionCheck(t, dir, "--base", "main", "--title", "feat(cli): rework the selection"); err == nil {
		t.Fatal("VersionCheck passed without the body: the footer counts only when the body is given")
	}
}

// A title that is not a conventional commit declares nothing, whatever its
// body says: the version bump reads the squash commit the same way.
func TestVersionCheckFailsATitleThatIsNotAConventionalCommit(t *testing.T) {
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("BREAKING CHANGE: --all is gone.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runVersionCheck(t, dir, "--base", "main", "--title", "Rework the selection", "--body-file", body)
	if err == nil || !strings.Contains(err.Error(), "not a conventional commit") {
		t.Fatalf("error = %v, want a failure that says the title is not a conventional commit", err)
	}
}

// Only the commits of the branch count: a breaking commit the base already
// holds was declared by its own squash commit.
func TestVersionCheckIgnoresTheCommitsOfTheBase(t *testing.T) {
	dir, _ := branchWithCommits(t, "feat(cli): add a flag")
	gitInRepo(t, dir, "checkout", "main")
	writeRepoFile(t, dir, "typescript/web/src.ts", "export const x = 1\n")
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-m", "feat(web)!: rename x")
	gitInRepo(t, dir, "checkout", "change")
	gitInRepo(t, dir, "merge", "--no-ff", "-m", "Merge branch 'main' into change", "main")

	if _, err := runVersionCheck(t, dir, "--base", "main", "--title", "feat(cli): add a flag"); err != nil {
		t.Fatalf("VersionCheck counted a breaking commit of the base: %v", err)
	}
}

func TestVersionCheckRefusesIncompleteArguments(t *testing.T) {
	dir, _ := branchWithCommits(t, "feat(cli): add a flag")
	for name, args := range map[string][]string{
		"no base":          {"--title", "feat: add"},
		"no title":         {"--base", "main"},
		"blank title":      {"--base", "main", "--title", " "},
		"missing value":    {"--base", "main", "--title"},
		"unknown argument": {"--base", "main", "--title", "feat: add", "extra"},
		"unreadable body":  {"--base", "main", "--title", "feat: add", "--body-file", filepath.Join(dir, "missing.md")},
	} {
		_, err := runVersionCheck(t, dir, args...)
		if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
			t.Errorf("%s: exit code = %d (%v), want the usage code %d", name, code, err, protocolcli.ExitUsage)
		}
	}
	if _, err := runVersionCheck(t, dir, "--base=no-such-ref", "--title=feat: add"); err == nil {
		t.Error("VersionCheck passed with a base that names no commit")
	}
}
