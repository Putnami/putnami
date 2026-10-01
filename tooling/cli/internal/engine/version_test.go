package engine

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// pkgJSONWithOverrides is the exact package.json shape that triggers a false
// dirty-suffix version stamp: a
// root that pins @putnami/cloud as both a direct dependency and an override
// (the EOVERRIDE bait) plus a non-conflicting yaml override.
const pkgJSONWithOverrides = `{
  "name": "workspace",
  "dependencies": {
    "@putnami/cloud": "0.0.0-abc"
  },
  "overrides": {
    "@putnami/cloud": "0.0.0-abc",
    "yaml": "^1.10.3"
  }
}
`

func commitPackageJSON(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSONWithOverrides), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLISelectionGit(t, dir, "add", "-A")
	runCLISelectionGit(t, dir, "commit", "-m", "add package.json with overrides")
}

// mustCaptureVersionSnapshot is captureVersionSnapshot for a test that sets no
// PUTNAMI_SOURCE_REVISION, where it cannot fail.
func mustCaptureVersionSnapshot(t *testing.T, dir string) *git.VersionInfo {
	t.Helper()
	snapshot, err := captureVersionSnapshot(dir)
	if err != nil {
		t.Fatalf("captureVersionSnapshot: %v", err)
	}
	return snapshot
}

// mustBuildVersionInfo is BuildVersionInfo for a test that sets no
// PUTNAMI_SOURCE_REVISION, where it cannot fail.
func mustBuildVersionInfo(t *testing.T, ws *workspace.Workspace) jobs.RunVersions {
	t.Helper()
	versions, err := BuildVersionInfo(ws)
	if err != nil {
		t.Fatalf("BuildVersionInfo: %v", err)
	}
	return versions
}

// snapshotWorkspace is the one-line workspace these tests stamp: version comes
// from git, so the workspace only has to name the line and its projects.
func snapshotWorkspace(t *testing.T, dir string) *workspace.Workspace {
	t.Helper()
	ws := workspace.NewWorkspace(dir, nil, []*workspace.Project{{ID: "/app", Name: "app", Path: "."}})
	ws.Lines = map[string]string{"": "v{version}"}
	return ws
}

// TestVersionSnapshot_CleanAcrossInRunMutation is a regression test: the
// version stamp captured at command start must stay clean even if a later step
// (a before-hook, codegen, or any tracked-file write) dirties the working tree
// during the run. Without the snapshot, git status sees that transient edit and
// folds it into a bogus "-<dirtyhash>" suffix on an otherwise clean checkout.
func TestVersionSnapshot_CleanAcrossInRunMutation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initCLISelectionGitRepo(t, dir)
	commitPackageJSON(t, dir)
	ws := snapshotWorkspace(t, dir)

	// The tree is clean here — this is what the user left behind.
	snapshot := mustCaptureVersionSnapshot(t, dir)
	if snapshot == nil {
		t.Fatal("captureVersionSnapshot returned nil on a clean git repo")
	}
	if snapshot.IsDirty {
		t.Fatalf("snapshot is dirty on a clean tree: suffix=%q", snapshot.Suffix)
	}

	// The stamp derived from the pre-mutation snapshot is the reference we expect
	// to survive the mutation, byte-for-byte.
	req := &Request{VersionSnapshot: snapshot}
	wantClean := req.runVersions(ws)[""]
	if wantClean == nil || wantClean.IsDirty {
		t.Fatalf("clean-tree stamp unexpectedly nil/dirty: %+v", wantClean)
	}
	if want := "0.0.0-" + snapshot.Suffix; wantClean.Full != want {
		t.Fatalf("clean-tree Full = %q, want %q", wantClean.Full, want)
	}

	// Simulate an in-run mutation that dirties a tracked file (e.g. a codegen job
	// rewriting package.json during the build).
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSONWithOverrides+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status := runCLISelectionGit(t, dir, "status", "--porcelain"); status == "" {
		t.Fatal("expected the in-run mutation to dirty the working tree, but git status is empty")
	}

	// Recomputing from the current tree now WOULD go dirty — this is exactly the
	// pre-fix behavior we are guarding against.
	if fresh := mustBuildVersionInfo(t, ws)[""]; fresh == nil || !fresh.IsDirty {
		t.Fatalf("expected a fresh version read after the mutation to be dirty (proving the bug); got %+v", fresh)
	}

	// The fix: runVersions reuses the pre-mutation snapshot and stays clean.
	got := req.runVersions(ws)[""]
	if got == nil {
		t.Fatal("runVersions returned no root line")
	}
	if got.IsDirty {
		t.Errorf("version stamp is dirty across the mutation: suffix=%q", got.Suffix)
	}
	if got.Suffix != wantClean.Suffix {
		t.Errorf("suffix changed across the mutation: got %q, want %q", got.Suffix, wantClean.Suffix)
	}
	if got.Full != wantClean.Full {
		t.Errorf("Full changed across the mutation: got %q, want %q", got.Full, wantClean.Full)
	}
}

// TestVersionSnapshot_DirtyTreeStillDirty confirms the snapshot does not mask a
// genuinely dirty checkout: a real uncommitted change made by the user must
// still produce a "-<dirtyhash>" suffix.
func TestVersionSnapshot_DirtyTreeStillDirty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initCLISelectionGitRepo(t, dir)
	commitPackageJSON(t, dir)
	ws := snapshotWorkspace(t, dir)

	// A real, user-made uncommitted change before capture.
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot := mustCaptureVersionSnapshot(t, dir)
	if snapshot == nil {
		t.Fatal("captureVersionSnapshot returned nil")
	}
	if !snapshot.IsDirty {
		t.Fatal("expected a genuinely dirty tree to be reported dirty")
	}

	got := (&Request{VersionSnapshot: snapshot}).runVersions(ws)[""]
	if got == nil || !got.IsDirty {
		t.Fatalf("expected dirty stamp for a dirty tree, got %+v", got)
	}
	if got.Suffix == snapshot.SHA {
		t.Errorf("dirty stamp should carry a -<dirtyhash> suffix, got bare sha %q", got.Suffix)
	}
}

// A tagged commit takes its line's tag as the version, with no suffix at all:
// that commit IS the release.
func TestRunVersions_TaggedCommitTakesTheTag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initCLISelectionGitRepo(t, dir)
	commitPackageJSON(t, dir)
	runCLISelectionGit(t, dir, "tag", "-a", "v1.4.0", "-m", "release")
	ws := snapshotWorkspace(t, dir)

	got := (&Request{VersionSnapshot: mustCaptureVersionSnapshot(t, dir)}).runVersions(ws)[""]
	if got == nil || !got.Tagged || got.Tag != "v1.4.0" {
		t.Fatalf("version = %+v, want the tag at HEAD", got)
	}
	if got.Base != "1.4.0" || got.Full != "1.4.0" {
		t.Errorf("Base/Full = %q/%q, want the tag's version with no suffix", got.Base, got.Full)
	}
}

// TestRunVersions_NilSnapshotFallsBack verifies that callers outside the
// suspend window (nil snapshot) fall back to a fresh git read, preserving the
// prior behavior.
func TestRunVersions_NilSnapshotFallsBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initCLISelectionGitRepo(t, dir)
	commitPackageJSON(t, dir)
	ws := snapshotWorkspace(t, dir)

	got := (&Request{}).runVersions(ws)[""]
	want := mustBuildVersionInfo(t, ws)[""]
	if got == nil || want == nil {
		t.Fatalf("nil result: got=%+v want=%+v", got, want)
	}
	if *got != *want {
		t.Errorf("nil-snapshot fallback diverged from BuildVersionInfo:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestReleasePlanningAndEvidenceShareRunVersions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initCLISelectionGitRepo(t, dir)
	commitPackageJSON(t, dir)
	ws := snapshotWorkspace(t, dir)
	req := &Request{WorkspaceRoot: dir, Commands: []string{"build"}, VersionSnapshot: mustCaptureVersionSnapshot(t, dir)}
	options, err := buildReleaseSetOptions(req, ws)
	if err != nil {
		t.Fatal(err)
	}
	want := options.Versions[""]
	if want == nil || want.Tagged {
		t.Fatalf("initial version = %+v, want an untagged version", want)
	}

	// An in-run tag change cannot make evidence lookup key against a different
	// version from the plan. The next request must still observe that change.
	runCLISelectionGit(t, dir, "tag", "v1.4.0")
	recovery := newCachedObservationRecovery(req, ws, nil, nil)
	if got := recovery.runVersions()[""]; got == nil || *got != *want {
		t.Fatalf("evidence version = %+v, want the planned version %+v", got, want)
	}
	optionsAgain, err := buildReleaseSetOptions(req, ws)
	if err != nil {
		t.Fatal(err)
	}
	if got := optionsAgain.Versions[""]; got != want {
		t.Fatalf("same run re-resolved its version: got %+v, want snapshot %+v", got, want)
	}
	iteration := watchIterationRequest(req, nil, nil, false)
	for name, next := range map[string]*Request{
		"new run":         {WorkspaceRoot: dir, Commands: []string{"build"}, VersionSnapshot: mustCaptureVersionSnapshot(t, dir)},
		"watch iteration": &iteration,
	} {
		t.Run(name, func(t *testing.T) {
			optionsNext, err := buildReleaseSetOptions(next, ws)
			if err != nil {
				t.Fatal(err)
			}
			if got := optionsNext.Versions[""]; got == nil || !got.Tagged || got.Full != "1.4.0" {
				t.Fatalf("new run version = %+v, want the new tag (GetVersionInfo error: %v)", got, next.versionsErr)
			}
		})
	}
}

// The publication-shaping parameters are read in exactly one place, so a flag
// that reaches the parser but not the coordinator is a defect this catches
// where it happens: `--baseline-channel` names the head a first publish into an
// empty channel measures against, and it has to arrive as one typed option
// beside the channels it is the baseline of.
func TestReleaseSetOptionsCarryTheBaselineChannelFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initCLISelectionGitRepo(t, dir)
	commitPackageJSON(t, dir)
	ws := snapshotWorkspace(t, dir)

	req := &Request{
		WorkspaceRoot: dir,
		Commands:      []string{"publish"},
		CommandParams: map[string]any{"channel": "pr-7", "baseline-channel": "canary"},
		Global:        GlobalFlags{Impacted: true},
	}
	options, err := buildReleaseSetOptions(req, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Channels) != 1 || options.Channels[0] != "pr-7" || options.BaselineChannel != "canary" {
		t.Fatalf("release-set options = %+v; want pr-7 advanced and canary as the baseline", options)
	}

	// The same value on a channel this publication advances is a usage error
	// at parse time, before anything is packaged.
	conflicting := *req
	conflicting.CommandParams = maps.Clone(req.CommandParams)
	conflicting.CommandParams["channel"] = "canary"
	if _, err := buildReleaseSetOptions(&conflicting, ws); err == nil {
		t.Fatal("a baseline channel this publication advances was accepted")
	}
}

// A degraded line is invisible in its 0.0.0 stamp, so --debug names why, one
// prefixed stderr line per version line; without --debug the run stays quiet.
// Not parallel: it redirects the process's stderr.
func TestRunVersionsDebugNamesEachDegradedLine(t *testing.T) {
	dir := t.TempDir() // outside any git repository: every line degrades
	ws := workspace.NewWorkspace(dir, nil, []*workspace.Project{
		{ID: "/go/api", Name: "api", Path: "go/api", Line: "go"},
		{ID: "/typescript/web", Name: "web", Path: "typescript/web", Line: "typescript"},
	})
	ws.Lines = map[string]string{"go": "go/v{version}", "typescript": "ts/v{version}"}
	snapshot := &git.VersionInfo{SHA: "cafe123", Branch: "main", Suffix: "20260102030405-cafe123"}

	quiet := captureStderr(t, func() {
		(&Request{VersionSnapshot: snapshot}).runVersions(ws)
	})
	if strings.Contains(quiet, "version line degraded") {
		t.Fatalf("degraded lines printed without --debug:\n%s", quiet)
	}

	req := &Request{VersionSnapshot: snapshot, Global: GlobalFlags{Debug: true}}
	out := captureStderr(t, func() { req.runVersions(ws) })
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "version line degraded") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "[debug] ") || !strings.HasPrefix(lines[1], "[debug] ") ||
		!strings.Contains(out, `version line "go"`) || !strings.Contains(out, `version line "typescript"`) {
		t.Fatalf("--debug output = %q, want one [debug] line naming each degraded line", out)
	}
	if req.versionsErr == nil {
		t.Fatal("the request kept no reason for its degraded lines")
	}
}
