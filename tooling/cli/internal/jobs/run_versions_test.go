package jobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// runVersionsRepo is a repository of two lines, one of them tagged, so the two
// halves of the derivation — the tag's version and the computed advance — are
// both exercised in one workspace.
func runVersionsRepo(t *testing.T) *workspace.Workspace {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@t.com"}, {"config", "user.name", "T"},
		{"config", "commit.gpgsign", "false"}, {"checkout", "-b", "main"},
	} {
		runVersionsGit(t, dir, args...)
	}
	writeRunVersionsFile(t, dir, "typescript/web/src.ts", "export const x = 1\n")
	writeRunVersionsFile(t, dir, "go/api/main.go", "package main\n")
	runVersionsGit(t, dir, "add", "-A")
	runVersionsGit(t, dir, "commit", "-m", "feat: the workspace")
	writeRunVersionsFile(t, dir, "go/api/next.go", "package main\n")
	runVersionsGit(t, dir, "add", "-A")
	runVersionsGit(t, dir, "commit", "-m", "fix(api): stop the leak")
	// The typescript line is released AT HEAD; the go line has never been.
	runVersionsGit(t, dir, "tag", "-a", "ts/v0.4.0", "-m", "release")

	web := &workspace.Project{ID: "/typescript/web", Name: "web", Path: "typescript/web", Line: "typescript"}
	api := &workspace.Project{ID: "/go/api", Name: "api", Path: "go/api", Line: "go"}
	ws := workspace.NewWorkspace(dir, nil, []*workspace.Project{web, api})
	ws.Lines = map[string]string{"typescript": "ts/v{version}", "go": "go/v{version}"}
	return ws
}

// Each line gets its own version: the tagged one takes its tag with no suffix,
// the untagged one starts at 0.0.0 and carries the ordered suffix.
func TestBuildRunVersionsResolvesEachLineSeparately(t *testing.T) {
	t.Parallel()
	ws := runVersionsRepo(t)
	snapshot, err := putnamigit.TreeState(ws.Root)
	if err != nil {
		t.Fatalf("capture the tree state: %v", err)
	}

	versions, err := BuildRunVersions(ws, snapshot)
	if err != nil {
		t.Fatalf("a readable repository degraded a line: %v", err)
	}
	typescript, go_ := versions["typescript"], versions["go"]
	if typescript == nil || go_ == nil {
		t.Fatalf("versions = %+v, want one entry per line", versions)
	}
	if !typescript.Tagged || typescript.Tag != "ts/v0.4.0" || typescript.Full != "0.4.0" {
		t.Errorf("typescript = %+v, want the tag's version with no suffix", typescript)
	}
	if go_.Tagged || go_.Base != "0.0.0" || !strings.HasPrefix(go_.Full, "0.0.0-") {
		t.Errorf("go = %+v, want an untagged line at 0.0.0 plus the suffix", go_)
	}
	if go_.SHA != snapshot.SHA || go_.Branch != snapshot.Branch {
		t.Errorf("go tree state = %q/%q, want the snapshot's %q/%q", go_.SHA, go_.Branch, snapshot.SHA, snapshot.Branch)
	}
	if got := VersionInfoForProject(versions, ws.ProjectByID("/go/api")); got != go_ {
		t.Errorf("project version = %+v, want its own line's", got)
	}
}

// The tree state a run OBSERVED wins over the one git reports afterwards: a
// hook or a codegen job may have dirtied the tree since.
func TestBuildRunVersionsKeepsTheCapturedTreeState(t *testing.T) {
	t.Parallel()
	ws := runVersionsRepo(t)
	snapshot := &putnamigit.VersionInfo{SHA: "cafe123", Branch: "main", Suffix: "20260102030405-cafe123"}

	versions, err := BuildRunVersions(ws, snapshot)
	if err != nil {
		t.Fatalf("a readable repository degraded a line: %v", err)
	}
	untagged := versions["go"]
	if untagged.Suffix != snapshot.Suffix || untagged.Full != "0.0.0-"+snapshot.Suffix {
		t.Fatalf("go = %+v, want the captured suffix", untagged)
	}
	// A tagged line keeps its suffix-free Full even though a snapshot rides on it.
	if versions["typescript"].Full != "0.4.0" {
		t.Fatalf("typescript = %+v, want the tag's version untouched", versions["typescript"])
	}
}

// An ordinary build in a checkout git cannot answer for must keep working:
// a build is not a release. The paths that DO release refuse it explicitly.
func TestBuildRunVersionsDegradesOutsideAGitRepository(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace(dir, nil, []*workspace.Project{project})
	snapshot := &putnamigit.VersionInfo{SHA: "cafe123", Branch: "main", Suffix: "20260102030405-cafe123"}

	versions, err := BuildRunVersions(ws, snapshot)
	if got := versions[""]; got == nil || got.Base != "0.0.0" || got.Full != "0.0.0-"+snapshot.Suffix {
		t.Fatalf("root line = %+v, want a degraded 0.0.0 stamp", got)
	}
	// The degraded stamp alone cannot say why; the error names the line and
	// says the root is outside every repository.
	if err == nil || !strings.Contains(err.Error(), `version line ""`) || !errors.Is(err, putnamigit.ErrNotRepository) {
		t.Fatalf("degradation error = %v, want the line and the missing repository", err)
	}
	// Without a snapshot the line is left out, and the error still says why.
	if got, err := BuildRunVersions(ws, nil); len(got) != 0 || err == nil {
		t.Fatalf("versions without a snapshot = %+v, %v, want none and the reason", got, err)
	}
	if got, err := BuildRunVersions(nil, snapshot); got != nil || err != nil {
		t.Fatalf("versions without a workspace = %+v, %v, want nil", got, err)
	}
}

// RequireFullClone is the explicit refusal the release paths make, so a
// publication never stamps a version computed from a fraction of the history.
func TestRequireFullClone(t *testing.T) {
	t.Parallel()
	ws := runVersionsRepo(t)
	if err := RequireFullClone(ws.Root); err != nil {
		t.Fatalf("RequireFullClone on a full clone = %v", err)
	}
	shallow := filepath.Join(t.TempDir(), "shallow")
	cmd := exec.Command("git", "clone", "--depth", "1", "file://"+ws.Root, shallow)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("shallow clone unavailable in this environment: %v\n%s", err, out)
	}
	err := RequireFullClone(shallow)
	if err == nil || !strings.Contains(err.Error(), "--unshallow") {
		t.Fatalf("RequireFullClone on a shallow clone = %v, want the refusal", err)
	}
}

func runVersionsGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z", "GIT_AUTHOR_DATE=2026-01-02T03:04:05Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeRunVersionsFile(t *testing.T, dir, relPath, contents string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// git's own stderr can span several lines (the refusal to read a repository
// another user owns is four), and each degraded version line must still print
// as exactly one line.
func TestDegradedVersionLinesRendersOneLinePerVersionLine(t *testing.T) {
	t.Parallel()
	ownership := errors.New("version line \"go\": fatal: detected dubious ownership in repository at '/w'\n" +
		"To add an exception for this directory, call:\n\n\tgit config --global --add safe.directory /w")
	shallow := errors.New(`version line "typescript": version and publish need a full clone with tags`)

	got := DegradedVersionLines(errors.Join(ownership, shallow))
	want := []string{
		`version line "go": fatal: detected dubious ownership in repository at '/w' To add an exception for this directory, call: git config --global --add safe.directory /w`,
		`version line "typescript": version and publish need a full clone with tags`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("DegradedVersionLines = %q, want %q", got, want)
	}
	if got := DegradedVersionLines(shallow); !slices.Equal(got, want[1:]) {
		t.Fatalf("a single unjoined error = %q, want it as one line", got)
	}
	if got := DegradedVersionLines(nil); got != nil {
		t.Fatalf("no degradation = %q, want nothing", got)
	}
}
