package agentartifacts

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// A fresh workspace ends up holding exactly the artifact, and the ownership
// state records exactly what was written.
func TestMaterialize_FreshWorkspace(t *testing.T) {
	files := defaultWorkflowFiles()
	fixture := newFixture(t, "1.0.0", files)

	report := fixture.mustMaterialize(t)
	if !report.Applied {
		t.Fatal("a clean run must report itself applied")
	}

	for path, content := range files {
		if got := fixture.read(t, path); got != content {
			t.Errorf("%s = %q, want %q", path, got, content)
		}
	}

	state, err := LoadState(fixture.root, testArtifactName)
	if err != nil {
		t.Fatal(err)
	}
	if state.ArtifactVersion != "1.0.0" || state.ArchiveDigest != fixture.entry.Integrity ||
		state.ManifestHash != fixture.entry.ManifestHash {
		t.Fatalf("state does not record the installed pin: %+v", state)
	}
	managed := state.ManagedDigests()
	if len(managed) != len(files) {
		t.Fatalf("state records %d managed files, want %d", len(managed), len(files))
	}
	for path, content := range files {
		if managed[path] != lockfile.HashBytes([]byte(content)) {
			t.Errorf("state digest for %s = %q", path, managed[path])
		}
	}
}

// Repeated installation is a no-op: no bytes move, no file is rewritten, and
// the state is unchanged.
func TestMaterialize_RepeatIsANoOp(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "idempotent", "a-repeated-run-changes-no-byte")
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.mustMaterialize(t)

	stateBefore, err := os.ReadFile(StatePath(fixture.root, testArtifactName))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, fixture.root)
	stamps := map[string]string{}
	for path := range defaultWorkflowFiles() {
		stamps[path] = statOf(t, fixture.root, path)
	}

	second := fixture.mustMaterialize(t)

	assertSameTree(t, before, snapshotTree(t, fixture.root))
	if len(second.Unchanged) != len(defaultWorkflowFiles()) {
		t.Fatalf("second run should classify everything unchanged: %+v", second)
	}
	if len(second.Created)+len(second.Updated)+len(second.Removed)+len(second.Collided) != 0 {
		t.Fatalf("second run mutated something: %+v", second)
	}
	for path, want := range stamps {
		if got := statOf(t, fixture.root, path); got != want {
			t.Errorf("%s was rewritten by the no-op run", path)
		}
	}
	stateAfter, err := os.ReadFile(StatePath(fixture.root, testArtifactName))
	if err != nil {
		t.Fatal(err)
	}
	if string(stateAfter) != string(stateBefore) {
		t.Fatalf("a no-op run rewrote the ownership state:\n%s\n%s", stateBefore, stateAfter)
	}
}

// An upgrade touches only files it can PROVE it owns: it updates the changed
// managed file, removes the dropped managed file, creates the new one, and
// leaves an unrelated user file alone.
func TestMaterialize_UpgradeChangesOnlyProvenManagedFiles(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.mustMaterialize(t)
	fixture.write(t, ".agents/notes/my-notes.md", "mine\n")

	next := defaultWorkflowFiles()
	next[".agents/skills/fix/SKILL.md"] = "# fix\nversion two\n"
	delete(next, ".agents/skills/fix/references/fix-heavy.md")
	next[".agents/skills/plan/SKILL.md"] = "# plan\n"
	fixture.upgrade(t, "2.0.0", next)

	report := fixture.mustMaterialize(t)

	if want := []string{".agents/skills/plan/SKILL.md"}; !equalStrings(report.Created, want) {
		t.Errorf("created = %v, want %v", report.Created, want)
	}
	if want := []string{".agents/skills/fix/SKILL.md"}; !equalStrings(report.Updated, want) {
		t.Errorf("updated = %v, want %v", report.Updated, want)
	}
	if want := []string{".agents/skills/fix/references/fix-heavy.md"}; !equalStrings(report.Removed, want) {
		t.Errorf("removed = %v, want %v", report.Removed, want)
	}
	if len(report.Unchanged) != 3 {
		t.Errorf("unchanged = %v, want the three carried-over files", report.Unchanged)
	}
	if got := fixture.read(t, ".agents/notes/my-notes.md"); got != "mine\n" {
		t.Errorf("an unrelated user file was touched: %q", got)
	}

	state, err := LoadState(fixture.root, testArtifactName)
	if err != nil {
		t.Fatal(err)
	}
	if state.ArtifactVersion != "2.0.0" {
		t.Errorf("state version = %q, want 2.0.0", state.ArtifactVersion)
	}
	if _, still := state.ManagedDigests()[".agents/skills/fix/references/fix-heavy.md"]; still {
		t.Error("the removed file is still recorded as managed")
	}
}

// Interruption recovery, in the two shapes it actually takes.
func TestMaterialize_InterruptedRunIsSafeToRerun(t *testing.T) {
	t.Run("files written, state never committed", func(t *testing.T) {
		fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
		artifact, plan, err := fixture.preflight(t)
		if err != nil {
			t.Fatal(err)
		}
		// Apply without WriteState is exactly the crash window between the last
		// rename and the state commit.
		if err := Apply(fixture.root, artifact, plan); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(StatePath(fixture.root, testArtifactName)); !os.IsNotExist(err) {
			t.Fatalf("precondition: state should not exist yet (%v)", err)
		}

		before := snapshotTree(t, fixture.root)
		report := fixture.mustMaterialize(t)
		assertSameTree(t, before, snapshotTree(t, fixture.root))

		if len(report.Unchanged) != len(defaultWorkflowFiles()) || len(report.Collided) != 0 {
			t.Fatalf("rerun after an interrupted run must converge silently: %+v", report)
		}
		if _, err := LoadState(fixture.root, testArtifactName); err != nil {
			t.Fatalf("the rerun must commit the ownership state: %v", err)
		}
	})

	t.Run("only some files written", func(t *testing.T) {
		files := defaultWorkflowFiles()
		fixture := newFixture(t, "1.0.0", files)
		// Half a transaction: one target already carries the artifact bytes.
		fixture.write(t, ".claude/skills/fix/SKILL.md", files[".claude/skills/fix/SKILL.md"])

		report := fixture.mustMaterialize(t)
		if !contains(report.Unchanged, ".claude/skills/fix/SKILL.md") {
			t.Errorf("the already-written file must re-plan as unchanged: %+v", report)
		}
		if len(report.Created) != len(files)-1 {
			t.Errorf("created = %v, want the remaining files", report.Created)
		}
		for path, content := range files {
			if got := fixture.read(t, path); got != content {
				t.Errorf("%s = %q, want %q", path, got, content)
			}
		}
	})

	t.Run("upgrade interrupted mid-commit", func(t *testing.T) {
		fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
		fixture.mustMaterialize(t)

		next := defaultWorkflowFiles()
		next[".agents/skills/fix/SKILL.md"] = "# fix\nversion two\n"
		next[".claude/skills/fix/SKILL.md"] = "See the canonical skill (v2)\n"
		fixture.upgrade(t, "2.0.0", next)
		// One of the two renames landed before the crash.
		fixture.write(t, ".agents/skills/fix/SKILL.md", next[".agents/skills/fix/SKILL.md"])

		report := fixture.mustMaterialize(t)
		if !contains(report.Unchanged, ".agents/skills/fix/SKILL.md") {
			t.Errorf("the published half must re-plan as unchanged: %+v", report)
		}
		if !contains(report.Updated, ".claude/skills/fix/SKILL.md") {
			t.Errorf("the unpublished half must re-plan as an update: %+v", report)
		}
		for path, content := range next {
			if got := fixture.read(t, path); got != content {
				t.Errorf("%s = %q, want %q", path, got, content)
			}
		}
	})
}

// Apply stages every byte before it publishes any, so a staging failure leaves
// zero target-file mutations. The failure is injected by making one target's
// parent directory unwritable AFTER the plan was built.
func TestApply_StagingFailureLeavesZeroMutations(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "record-last", "a-failure-leaves-files-and-record-untouched")
	if os.Geteuid() == 0 {
		t.Skip("a root process ignores directory permissions")
	}
	files := defaultWorkflowFiles()
	fixture := newFixture(t, "1.0.0", files)
	// Pre-create one target with its artifact bytes so the plan carries an
	// unchanged entry too, and seed a second directory we can lock down.
	fixture.write(t, ".claude/skills/fix/SKILL.md", files[".claude/skills/fix/SKILL.md"])

	artifact, plan, err := fixture.preflight(t)
	if err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(fixture.root, ".agents", "skills", "fix")
	if runtime.GOOS == "windows" {
		// A read-only attribute does not deny creating files in a Windows
		// directory, so a regular file takes the directory's place instead:
		// the staging write fails on creating the target's directory.
		if err := os.MkdirAll(filepath.Dir(locked), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(locked, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	}

	before := snapshotTree(t, fixture.root)
	if err := Apply(fixture.root, artifact, plan); err == nil {
		t.Fatal("expected the staging write to fail")
	}
	if _, err := os.Lstat(StatePath(fixture.root, testArtifactName)); !os.IsNotExist(err) {
		t.Fatalf("a failed apply left an ownership record: %v", err)
	}
	assertSameTree(t, before, snapshotTree(t, fixture.root))
}

// A corrupt ownership state stops the run before anything is fetched-and-then-
// written, and the workspace is left exactly as it was.
func TestMaterialize_CorruptStateAbortsWithoutMutating(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.mustMaterialize(t)

	if err := os.WriteFile(StatePath(fixture.root, testArtifactName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := defaultWorkflowFiles()
	next[".agents/skills/fix/SKILL.md"] = "# fix\nversion two\n"
	fixture.upgrade(t, "2.0.0", next)

	before := snapshotTree(t, fixture.root)
	_, err := fixture.materialize(t)
	var corrupt *CorruptStateError
	if !errors.As(err, &corrupt) {
		t.Fatalf("error = %v, want a *CorruptStateError", err)
	}
	assertSameTree(t, before, snapshotTree(t, fixture.root))
}

// A verification failure upstream of the plan surfaces as an error, never as
// an empty plan that would look like "the content ships nothing" and remove
// every managed file.
func TestMaterialize_VerificationFailureTouchesNothing(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.entry.ManifestHash = strings.Repeat("e", 64)
	if _, err := fixture.materialize(t); err == nil {
		t.Fatal("expected the manifest binding to fail")
	}
	if _, err := os.Lstat(filepath.Join(fixture.root, ".agents")); !os.IsNotExist(err) {
		t.Errorf("a failed verification must not touch the workspace: %v", err)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
