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

// writeSquashMessage writes a squash commit message file and returns its path.
func writeSquashMessage(t *testing.T, message string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "squash-message.txt")
	if err := os.WriteFile(path, []byte(message), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The body counts only when the caller writes it in the message: a host that
// squashes with the pull request body keeps its footer in the commit
// `putnami version` reads.
func TestVersionCheckCountsABreakingFooterInTheMessageFile(t *testing.T) {
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")
	// A host returns a body with CRLF line ends.
	withFooter := writeSquashMessage(t, "feat(cli): rework the selection\n\nDrop the flag.\r\n\r\nBREAKING CHANGE: --all is gone.\r\n")
	titleAlone := writeSquashMessage(t, "feat(cli): rework the selection\n\n\n")

	if _, err := runVersionCheck(t, dir, "--base", "main", "--message-file", withFooter); err != nil {
		t.Fatalf("VersionCheck with a breaking footer in the body: %v", err)
	}
	_, err := runVersionCheck(t, dir, "--base", "main", "--message-file", titleAlone)
	if err == nil || !strings.Contains(err.Error(), `"feat(cli): rework the selection"`) {
		t.Fatalf("error = %v, want a failure that quotes the subject of the message file", err)
	}
}

// Git reads a line directly under the first one as part of the subject, so a
// footer written there is in no body: the check reads the message as git will.
func TestVersionCheckReadsAFooterWithoutABlankLineAsTheSubject(t *testing.T) {
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")
	message := writeSquashMessage(t, "feat(cli): rework the selection\nBREAKING CHANGE: --all is gone.\n")

	_, err := runVersionCheck(t, dir, "--base", "main", "--message-file", message)
	if err == nil || !strings.Contains(err.Error(), `"feat(cli): rework the selection BREAKING CHANGE: --all is gone."`) {
		t.Fatalf("error = %v, want a failure that quotes the subject git would keep", err)
	}
}

// A title that is not a conventional commit declares nothing, whatever its
// body says: the version bump reads the squash commit the same way.
func TestVersionCheckFailsATitleThatIsNotAConventionalCommit(t *testing.T) {
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")
	message := writeSquashMessage(t, "Rework the selection\n\nBREAKING CHANGE: --all is gone.\n")

	_, err := runVersionCheck(t, dir, "--base", "main", "--message-file", message)
	if err == nil || !strings.Contains(err.Error(), "not a conventional commit") {
		t.Fatalf("error = %v, want a failure that says the title is not a conventional commit", err)
	}
}

// A shallow clone holds part of the range: the check refuses it instead of
// passing without the commit that declares the break.
func TestVersionCheckRefusesAShallowClone(t *testing.T) {
	spectest.Proves(t, "cli/version-from-git", "squash-keeps-the-breaking-marker", "shallow-clone-is-refused-by-the-check")
	origin, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag", "fix(cli): stop the leak")
	clone := filepath.Join(t.TempDir(), "clone")
	gitInRepo(t, t.TempDir(), "clone", "--depth", "1", "--no-single-branch", "file://"+origin, clone)

	_, err := runVersionCheck(t, clone, "--base", "origin/main", "--title", "feat(cli): rework the selection")
	if err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Fatalf("error = %v, want a refusal that names the shallow clone", err)
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
	empty := writeSquashMessage(t, "\nfeat: add\n")
	valid := writeSquashMessage(t, "feat: add\n")
	for name, args := range map[string][]string{
		"no base":            {"--title", "feat: add"},
		"no title":           {"--base", "main"},
		"blank title":        {"--base", "main", "--title", " "},
		"missing value":      {"--base", "main", "--title"},
		"unknown argument":   {"--base", "main", "--title", "feat: add", "extra"},
		"title and message":  {"--base", "main", "--title", "feat: add", "--message-file", valid},
		"unreadable message": {"--base", "main", "--message-file", filepath.Join(dir, "missing.txt")},
		"empty first line":   {"--base", "main", "--message-file", empty},
	} {
		_, err := runVersionCheck(t, dir, args...)
		if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
			t.Errorf("%s: exit code = %d (%v), want the usage code %d", name, code, err, protocolcli.ExitUsage)
		}
	}
	if _, err := runVersionCheck(t, dir, "--base", "no-such-ref", "--title", "feat: add"); err == nil {
		t.Error("VersionCheck passed with a base that names no commit")
	}
}
