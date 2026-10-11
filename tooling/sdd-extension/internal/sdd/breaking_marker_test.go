package sdd

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

const breakingMarkerFeature = "tooling/specification-driven-development"
const breakingMarkerRequirement = "squash-keeps-the-breaking-marker"

// branchWithCommits is a repository whose branch "change" holds one commit per
// message on top of main. It returns the directory and the commit IDs, oldest
// first.
func branchWithCommits(t *testing.T, messages ...string) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := decisionsWorkspace(t)
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"checkout", "-q", "-b", "main"},
	} {
		gitIn(t, dir, args...)
	}
	commitFile(t, dir, "README.md", "# Test\n", "chore: start")
	gitIn(t, dir, "checkout", "-q", "-b", "change")
	shas := make([]string, 0, len(messages))
	for i, message := range messages {
		shas = append(shas, commitFile(t, dir, "src.go", "package src\n\nconst step = "+strconv.Itoa(i)+"\n", message))
	}
	return dir, shas
}

// commitFile writes one file, commits it with message, and returns the commit
// ID.
func commitFile(t *testing.T, dir, name, content, message string) string {
	t.Helper()
	writeFixtureFile(t, filepath.Join(dir, name), content)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", message)
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// The squash commit keeps the title and drops the branch commits, so a marker
// that only a branch commit carries is lost: the step fails and names it.
func TestBreakingMarkerFailsATitleThatDropsTheMarkerAndNamesTheCommit(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-dropped-marker-fails-naming-the-commit")
	dir, shas := branchWithCommits(t, "fix(cli): stop the leak", "feat(cli)!: drop the --all flag")

	report, err := BuildBreakingMarkerResult(dir, "feat(cli): rework the selection", "main")
	if err == nil {
		t.Fatal("the step passed a title without the marker a branch commit declares")
	}
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d, the code of every validate-workspace finding", code, protocolcli.ExitUsage)
	}
	message := err.Error()
	if want := shas[1][:12] + " feat(cli)!: drop the --all flag"; !strings.Contains(message, want) {
		t.Errorf("error = %q, want it to name the breaking commit %q", message, want)
	}
	if strings.Contains(message, shas[0][:12]) {
		t.Errorf("error = %q, names the commit %s that declares nothing", message, shas[0][:12])
	}
	if !strings.Contains(message, `Add "!" before the colon of the title`) || !strings.Contains(message, "reword the commit") {
		t.Errorf("error = %q, want it to say how to declare the break or reword the commit", message)
	}
	if report.Commits != 2 || len(report.Breaking) != 1 || report.Breaking[0].Commit != shas[1][:12] {
		t.Errorf("report = %+v, want 2 commits read and the one breaking commit listed", report)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != ErrorCodeBreakingMarkerDropped ||
		!strings.Contains(report.Diagnostics[0].Message, shas[1][:12]) {
		t.Errorf("diagnostics = %+v, want one %s finding naming the commit", report.Diagnostics, ErrorCodeBreakingMarkerDropped)
	}
	if ResultData(err) == nil {
		t.Error("the failure carries no report")
	}
}

// Every breaking commit is named, not only the first one.
func TestBreakingMarkerNamesEveryBreakingCommit(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-dropped-marker-fails-naming-the-commit")
	dir, shas := branchWithCommits(t, "feat(cli)!: drop the --all flag", "fix(api)!: rename the field")

	_, err := BuildBreakingMarkerResult(dir, "feat(cli): rework the selection", "main")
	if err == nil {
		t.Fatal("the step passed a title without the marker two branch commits declare")
	}
	for _, sha := range shas {
		if !strings.Contains(err.Error(), sha[:12]) {
			t.Errorf("error = %q, want it to name %s", err, sha[:12])
		}
	}
	if !strings.Contains(err.Error(), "2 commits declare") {
		t.Errorf("error = %q, want it to count both commits", err)
	}
}

func TestBreakingMarkerPassesATitleThatCarriesTheMarker(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-title-with-the-marker-passes")
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")

	report, err := BuildBreakingMarkerResult(dir, "feat(cli)!: rework the selection", "main")
	if err != nil {
		t.Fatalf("BuildBreakingMarkerResult: %v", err)
	}
	if !report.Declares || len(report.Breaking) != 1 || len(report.Diagnostics) != 0 {
		t.Errorf("report = %+v, want a declaring message, one breaking commit and no finding", report)
	}
}

// A title passes whatever it says when no commit of the change declares a
// break, a title that is not a conventional commit included.
func TestBreakingMarkerPassesAnyTitleWithoutABreakingCommit(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "no-breaking-commit-passes-any-title")
	dir, _ := branchWithCommits(t, "feat(cli): add a flag", "fix(cli): stop the leak")

	for _, title := range []string{"feat(cli): add a flag", "feat(cli)!: add a flag", "Add a flag", "WIP"} {
		report, err := BuildBreakingMarkerResult(dir, title, "main")
		if err != nil {
			t.Errorf("title %q: %v", title, err)
		}
		if report.Commits != 2 || len(report.Breaking) != 0 {
			t.Errorf("title %q: report = %+v, want 2 commits read and none breaking", title, report)
		}
	}
}

// A footer in a branch commit body declares a break as "!" does.
func TestBreakingMarkerReadsTheFooterOfABranchCommit(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-footer-counts-only-in-a-body")
	dir, shas := branchWithCommits(t, "refactor(cli): drop the greeting\n\nBREAKING CHANGE: Greet is gone; use Wave.")

	_, err := BuildBreakingMarkerResult(dir, "refactor(cli): drop the greeting", "main")
	if err == nil || !strings.Contains(err.Error(), shas[0][:12]) {
		t.Fatalf("error = %v, want a failure that names %s", err, shas[0][:12])
	}
}

// The body counts because the runner writes it into the message only when the
// repository squashes with it: then its footer lands in the commit
// `putnami version` reads.
func TestBreakingMarkerCountsAFooterInTheSquashBody(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-footer-counts-only-in-a-body")
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")

	// A pull request body arrives with CRLF line ends.
	withFooter := "feat(cli): rework the selection\n\nDrop the flag.\r\n\r\nBREAKING CHANGE: --all is gone.\r\n"
	if _, err := BuildBreakingMarkerResult(dir, withFooter, "main"); err != nil {
		t.Fatalf("a breaking footer in the squash body: %v", err)
	}
	_, err := BuildBreakingMarkerResult(dir, "feat(cli): rework the selection\n\n\n", "main")
	if err == nil || !strings.Contains(err.Error(), `"feat(cli): rework the selection"`) {
		t.Fatalf("error = %v, want a failure that quotes the subject of the message", err)
	}
}

// Git reads a line directly under the first one as part of the subject, so a
// footer written there is in no body: the step reads the message as Git will.
func TestBreakingMarkerReadsAFooterWithoutABlankLineAsTheSubject(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-footer-counts-only-in-a-body")
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")

	_, err := BuildBreakingMarkerResult(dir, "feat(cli): rework the selection\nBREAKING CHANGE: --all is gone.\n", "main")
	if err == nil || !strings.Contains(err.Error(), `"feat(cli): rework the selection BREAKING CHANGE: --all is gone."`) {
		t.Fatalf("error = %v, want a failure that quotes the subject Git would keep", err)
	}
}

// A title that is not a conventional commit declares nothing, whatever its body
// says: the version bump reads the squash commit the same way.
func TestBreakingMarkerFailsATitleThatIsNotAConventionalCommit(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-non-conventional-title-declares-nothing")
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")

	report, err := BuildBreakingMarkerResult(dir, "Rework the selection\n\nBREAKING CHANGE: --all is gone.\n", "main")
	if err == nil || !strings.Contains(err.Error(), "not a conventional commit, so nothing in the body counts") {
		t.Fatalf("error = %v, want a failure that says the title is not a conventional commit", err)
	}
	if report.Conventional || report.Declares {
		t.Errorf("report = %+v, want a non-conventional message that declares nothing", report)
	}
}

// Without a proposal there is nothing to compare: the step passes, says so, and
// reads no history, so a run outside a pull request needs no baseline.
func TestBreakingMarkerPassesWithANoteWhenNoMessageIsSet(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "an-unset-message-passes-with-a-note")
	for _, message := range []string{"", " \n\t\n"} {
		report, err := BuildBreakingMarkerResult(t.TempDir(), message, "")
		if err != nil {
			t.Fatalf("message %q: %v", message, err)
		}
		if report.Proposal || !strings.Contains(report.Note, "no proposal message to compare") {
			t.Errorf("message %q: report = %+v, want a note that nothing was compared", message, report)
		}
	}
}

// A message with no baseline cannot be compared with anything: the step fails
// rather than passing on an empty range.
func TestBreakingMarkerRefusesAMessageWithoutABaseline(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-missing-baseline-fails")
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")

	for _, title := range []string{"feat(cli): rework the selection", "feat(cli)!: rework the selection"} {
		_, err := BuildBreakingMarkerResult(dir, title, " ")
		if err == nil || !strings.Contains(err.Error(), "run with --impacted") {
			t.Fatalf("title %q: error = %v, want a refusal that asks for an --impacted run", title, err)
		}
		if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
			t.Errorf("title %q: exit code = %d, want %d", title, code, protocolcli.ExitUsage)
		}
	}
}

// A shallow clone holds part of the range: the step refuses it instead of
// passing without the commit that declares the break.
func TestBreakingMarkerRefusesAShallowClone(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "a-shallow-clone-fails")
	origin, shas := branchWithCommits(t, "feat(cli)!: drop the --all flag", "fix(cli): stop the leak")
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, t.TempDir(), "clone", "-q", "--depth", "1", "--no-single-branch", "file://"+origin, clone)
	gitIn(t, clone, "checkout", "-q", "change")

	// The fixture proves something only if the clone really lacks the
	// breaking commit: read it as an unguarded reader would.
	log := exec.Command("git", "log", "--format=%H", "origin/main..HEAD")
	log.Dir = clone
	out, err := log.Output()
	if err != nil {
		t.Fatalf("git log in the shallow clone: %v", err)
	}
	if strings.Contains(string(out), shas[0]) {
		t.Fatalf("the shallow clone holds the breaking commit %s; the fixture would not prove the refusal", shas[0])
	}

	for _, title := range []string{"feat(cli): rework the selection", "feat(cli)!: rework the selection"} {
		_, err := BuildBreakingMarkerResult(clone, title, "origin/main")
		if err == nil || !strings.Contains(err.Error(), "shallow clone") {
			t.Fatalf("title %q: error = %v, want a refusal that names the shallow clone", title, err)
		}
	}
}

// Only the commits of the change count: a breaking commit the base already
// holds was declared by its own squash commit, and merging the base into the
// branch does not bring it into the range.
func TestBreakingMarkerIgnoresTheCommitsOfAMergedBase(t *testing.T) {
	spectest.Proves(t, breakingMarkerFeature, breakingMarkerRequirement, "base-commits-merged-in-do-not-count")
	dir, _ := branchWithCommits(t, "feat(cli): add a flag")
	gitIn(t, dir, "checkout", "-q", "main")
	commitFile(t, dir, "web.ts", "export const x = 1\n", "feat(web)!: rename x")
	gitIn(t, dir, "checkout", "-q", "change")
	gitIn(t, dir, "merge", "-q", "--no-ff", "-m", "Merge branch 'main' into change", "main")

	report, err := BuildBreakingMarkerResult(dir, "feat(cli): add a flag", "main")
	if err != nil {
		t.Fatalf("the step counted a breaking commit of the base: %v", err)
	}
	if report.Commits != 2 || len(report.Breaking) != 0 {
		t.Errorf("report = %+v, want the branch commit and the merge read, and neither breaking", report)
	}
}

// A baseline that names no commit is an error, never an empty range that
// passes.
func TestBreakingMarkerFailsABaselineThatNamesNoCommit(t *testing.T) {
	dir, _ := branchWithCommits(t, "feat(cli)!: drop the --all flag")
	if _, err := BuildBreakingMarkerResult(dir, "feat(cli): rework the selection", "origin/missing"); err == nil {
		t.Fatal("the step passed with a baseline that names no commit")
	}
	if _, err := BuildBreakingMarkerResult(t.TempDir(), "feat(cli): rework the selection", "main"); err == nil {
		t.Fatal("the step passed outside a repository")
	}
}

func TestSplitCommitMessageReadsTheMessageAsGitDoes(t *testing.T) {
	for _, tc := range []struct {
		message, subject, body string
	}{
		{"feat: add", "feat: add", ""},
		{"feat: add\n", "feat: add", ""},
		{"feat: add\n\nbody line\n\nBREAKING CHANGE: x", "feat: add", "body line\n\nBREAKING CHANGE: x"},
		{"feat: add\r\n\r\nbody\r\n", "feat: add", "body\n"},
		{"feat: add\nsecond line\n\nbody", "feat: add second line", "body"},
		{"\n\n  feat!: add  \n\nbody", "feat!: add", "body"},
		{"feat: add\n \t\nbody", "feat: add", "body"},
	} {
		subject, body := splitCommitMessage(tc.message)
		if subject != tc.subject || body != tc.body {
			t.Errorf("splitCommitMessage(%q) = %q, %q; want %q, %q", tc.message, subject, body, tc.subject, tc.body)
		}
	}
}
