package agentartifacts

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The ownership table has seven rows and each one gets its own test below, in
// the order the issue states them. They are separate tests rather than a table
// because each row needs a different workspace shape, and because a regression
// should name the row it broke.

// Row 1: missing -> create.
func TestPlan_MissingTargetIsCreated(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())

	report := fixture.mustMaterialize(t)

	if len(report.Created) != len(defaultWorkflowFiles()) {
		t.Fatalf("created = %v, want every declared file", report.Created)
	}
	if len(report.Updated)+len(report.Removed)+len(report.Unchanged)+len(report.Collided) != 0 {
		t.Fatalf("a fresh install must only create: %+v", report)
	}
	if got := fixture.read(t, ".agents/skills/fix/SKILL.md"); got != defaultWorkflowFiles()[".agents/skills/fix/SKILL.md"] {
		t.Fatalf("materialized content = %q", got)
	}
}

// Row 2: existing and byte-identical -> no-op. The file is adopted without a
// write even though no previous run recorded it, which is also what makes an
// interrupted run safe to rerun.
func TestPlan_ByteIdenticalTargetIsUnchanged(t *testing.T) {
	files := defaultWorkflowFiles()
	fixture := newFixture(t, "1.0.0", files)
	fixture.write(t, ".agents/skills/fix/SKILL.md", files[".agents/skills/fix/SKILL.md"])

	before := statOf(t, fixture.root, ".agents/skills/fix/SKILL.md")
	report := fixture.mustMaterialize(t)

	if !contains(report.Unchanged, ".agents/skills/fix/SKILL.md") {
		t.Fatalf("unchanged = %v, want the byte-identical file", report.Unchanged)
	}
	if contains(report.Created, ".agents/skills/fix/SKILL.md") || contains(report.Updated, ".agents/skills/fix/SKILL.md") {
		t.Fatalf("a byte-identical file must not be rewritten: %+v", report)
	}
	if after := statOf(t, fixture.root, ".agents/skills/fix/SKILL.md"); after != before {
		t.Fatalf("byte-identical file was rewritten (%s -> %s)", before, after)
	}
}

// Row 3: existing, recorded managed, still the recorded bytes -> update.
func TestPlan_ManagedAndUnmodifiedTargetIsUpdated(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.mustMaterialize(t)

	next := defaultWorkflowFiles()
	next[".agents/skills/fix/SKILL.md"] = "# fix\nversion two\n"
	fixture.upgrade(t, "2.0.0", next)

	report := fixture.mustMaterialize(t)

	if !contains(report.Updated, ".agents/skills/fix/SKILL.md") {
		t.Fatalf("updated = %v, want the changed file", report.Updated)
	}
	if got := fixture.read(t, ".agents/skills/fix/SKILL.md"); got != next[".agents/skills/fix/SKILL.md"] {
		t.Fatalf("content = %q, want the new artifact bytes", got)
	}
}

// Row 4: existing but unrecorded -> preserve and report a collision.
func TestPlan_UnrecordedTargetCollides(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "collision-aborts", "one-collision-aborts-before-the-first-write")
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.write(t, ".agents/skills/fix/SKILL.md", "# my own workflow\n")

	report, err := fixture.materialize(t)
	if !errors.Is(err, ErrCollision) {
		t.Fatalf("error = %v, want ErrCollision", err)
	}
	if got := collisionReason(report, ".agents/skills/fix/SKILL.md"); got != ReasonUnmanaged {
		t.Fatalf("reason = %q, want %q", got, ReasonUnmanaged)
	}
	if got := fixture.read(t, ".agents/skills/fix/SKILL.md"); got != "# my own workflow\n" {
		t.Fatalf("user file was modified: %q", got)
	}
}

// Row 5: recorded managed but locally modified -> preserve and report.
func TestPlan_LocallyModifiedManagedTargetCollides(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "collision-aborts", "one-collision-aborts-before-the-first-write")
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.mustMaterialize(t)
	fixture.write(t, ".agents/skills/fix/SKILL.md", "# fix\nlocally tuned\n")

	next := defaultWorkflowFiles()
	next[".agents/skills/fix/SKILL.md"] = "# fix\nversion two\n"
	fixture.upgrade(t, "2.0.0", next)

	report, err := fixture.materialize(t)
	if !errors.Is(err, ErrCollision) {
		t.Fatalf("error = %v, want ErrCollision", err)
	}
	if got := collisionReason(report, ".agents/skills/fix/SKILL.md"); got != ReasonModified {
		t.Fatalf("reason = %q, want %q", got, ReasonModified)
	}
	if got := fixture.read(t, ".agents/skills/fix/SKILL.md"); got != "# fix\nlocally tuned\n" {
		t.Fatalf("the local edit was lost: %q", got)
	}
}

// Row 6: dropped from the artifact and unchanged since install -> remove.
func TestPlan_DroppedAndUnmodifiedTargetIsRemoved(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.mustMaterialize(t)

	next := defaultWorkflowFiles()
	delete(next, ".agents/skills/fix/references/fix-heavy.md")
	fixture.upgrade(t, "2.0.0", next)

	report := fixture.mustMaterialize(t)

	if !contains(report.Removed, ".agents/skills/fix/references/fix-heavy.md") {
		t.Fatalf("removed = %v, want the dropped file", report.Removed)
	}
	if _, err := os.Lstat(filepath.Join(fixture.root, ".agents/skills/fix/references/fix-heavy.md")); !os.IsNotExist(err) {
		t.Fatalf("dropped file still present: %v", err)
	}
	// The directory it emptied is pruned, but only because nothing else is in it.
	if _, err := os.Lstat(filepath.Join(fixture.root, ".agents/skills/fix/references")); !os.IsNotExist(err) {
		t.Errorf("emptied directory should be pruned: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.root, ".agents/skills/fix/SKILL.md")); err != nil {
		t.Errorf("pruning must stop at the first non-empty directory: %v", err)
	}
}

// Row 7: dropped from the artifact but locally modified -> preserve and report.
func TestPlan_DroppedButModifiedTargetCollides(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.mustMaterialize(t)
	fixture.write(t, ".agents/skills/fix/references/fix-heavy.md", "# my notes\n")

	next := defaultWorkflowFiles()
	delete(next, ".agents/skills/fix/references/fix-heavy.md")
	fixture.upgrade(t, "2.0.0", next)

	report, err := fixture.materialize(t)
	if !errors.Is(err, ErrCollision) {
		t.Fatalf("error = %v, want ErrCollision", err)
	}
	if got := collisionReason(report, ".agents/skills/fix/references/fix-heavy.md"); got != ReasonModifiedRemoval {
		t.Fatalf("reason = %q, want %q", got, ReasonModifiedRemoval)
	}
	if got := fixture.read(t, ".agents/skills/fix/references/fix-heavy.md"); got != "# my notes\n" {
		t.Fatalf("the local edit was lost: %q", got)
	}
}

// A recorded path that is already gone is not a removal: nothing is mutated, so
// it appears in no report section — and the next state simply stops recording
// it. This is the recovery path for a run interrupted after a removal.
func TestPlan_AlreadyAbsentDroppedPathIsNotReported(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.mustMaterialize(t)
	if err := os.Remove(filepath.Join(fixture.root, ".agents/skills/fix/references/fix-heavy.md")); err != nil {
		t.Fatal(err)
	}

	next := defaultWorkflowFiles()
	delete(next, ".agents/skills/fix/references/fix-heavy.md")
	fixture.upgrade(t, "2.0.0", next)

	report := fixture.mustMaterialize(t)
	if len(report.Removed) != 0 || len(report.Collided) != 0 {
		t.Fatalf("an already-absent managed path must be silent: %+v", report)
	}
	state, err := LoadState(fixture.root, testArtifactName)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := state.ManagedDigests()[".agents/skills/fix/references/fix-heavy.md"]; still {
		t.Error("the dropped path must not stay in the ownership state")
	}
}

// A managed path that was REPLACED by something else is not the file the
// removal was authorized for, so the removal is refused. Deleting through a
// symlink here would remove whatever it points at.
func TestPlan_ReplacedManagedPathIsNotRemoved(t *testing.T) {
	dropped := ".agents/skills/fix/references/fix-heavy.md"

	t.Run("replaced by a symlink", func(t *testing.T) {
		requireSymlinks(t)
		fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
		fixture.mustMaterialize(t)

		outside := filepath.Join(t.TempDir(), "kept.md")
		if err := os.WriteFile(outside, []byte("kept\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(fixture.root, filepath.FromSlash(dropped))
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, target); err != nil {
			t.Fatal(err)
		}

		next := defaultWorkflowFiles()
		delete(next, dropped)
		fixture.upgrade(t, "2.0.0", next)

		report, err := fixture.materialize(t)
		if !errors.Is(err, ErrCollision) {
			t.Fatalf("error = %v, want ErrCollision", err)
		}
		if got := collisionReason(report, dropped); got != ReasonSymlink {
			t.Fatalf("reason = %q, want %q", got, ReasonSymlink)
		}
		if _, err := os.Lstat(outside); err != nil {
			t.Fatalf("the symlink target was deleted: %v", err)
		}
	})

	t.Run("replaced by a directory", func(t *testing.T) {
		fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
		fixture.mustMaterialize(t)

		target := filepath.Join(fixture.root, filepath.FromSlash(dropped))
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(target, "mine"), 0o755); err != nil {
			t.Fatal(err)
		}

		next := defaultWorkflowFiles()
		delete(next, dropped)
		fixture.upgrade(t, "2.0.0", next)

		report, err := fixture.materialize(t)
		if !errors.Is(err, ErrCollision) {
			t.Fatalf("error = %v, want ErrCollision", err)
		}
		if got := collisionReason(report, dropped); got != ReasonNotAFile {
			t.Fatalf("reason = %q, want %q", got, ReasonNotAFile)
		}
		if _, err := os.Lstat(filepath.Join(target, "mine")); err != nil {
			t.Fatalf("the replacing directory was destroyed: %v", err)
		}
	})

	t.Run("under a symlinked parent", func(t *testing.T) {
		requireSymlinks(t)
		fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
		fixture.mustMaterialize(t)

		references := filepath.Join(fixture.root, ".agents/skills/fix/references")
		relocated := t.TempDir()
		if err := os.Rename(filepath.Join(references, "fix-heavy.md"), filepath.Join(relocated, "fix-heavy.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(references); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(relocated, references); err != nil {
			t.Fatal(err)
		}

		next := defaultWorkflowFiles()
		delete(next, dropped)
		fixture.upgrade(t, "2.0.0", next)

		report, err := fixture.materialize(t)
		if !errors.Is(err, ErrCollision) {
			t.Fatalf("error = %v, want ErrCollision", err)
		}
		if got := collisionReason(report, dropped); got != ReasonSymlink {
			t.Fatalf("reason = %q, want %q", got, ReasonSymlink)
		}
		if _, err := os.Lstat(filepath.Join(relocated, "fix-heavy.md")); err != nil {
			t.Fatalf("a file behind a symlinked parent was removed: %v", err)
		}
	})
}

// workspacePath is the single place a declared or recorded string becomes a
// filesystem path, so it refuses anything the protocol rule rejects even when
// the caller reached it directly.
func TestWorkspacePath_RefusesUnsafeStrings(t *testing.T) {
	root := t.TempDir()
	for _, unsafe := range []string{"", "..", "../escape", "/etc/passwd", `a\b`, ".agents/../../x"} {
		if _, err := workspacePath(root, unsafe); err == nil {
			t.Errorf("workspacePath(%q) = nil error, want a refusal", unsafe)
		}
	}
	got, err := workspacePath(root, ".agents/skills/fix/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".agents", "skills", "fix", "SKILL.md"); got != want {
		t.Fatalf("workspacePath = %q, want %q", got, want)
	}
}

// A symlinked TARGET is never followed for a write: that is how a materializer
// is talked into writing outside the paths its manifest declares.
func TestPlan_SymlinkedTargetCollides(t *testing.T) {
	requireSymlinks(t)
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(fixture.root, ".agents/skills/fix/SKILL.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	report, err := fixture.materialize(t)
	if !errors.Is(err, ErrCollision) {
		t.Fatalf("error = %v, want ErrCollision", err)
	}
	if got := collisionReason(report, ".agents/skills/fix/SKILL.md"); got != ReasonSymlink {
		t.Fatalf("reason = %q, want %q", got, ReasonSymlink)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "outside\n" {
		t.Fatalf("the symlink target was written through: %q / %v", data, err)
	}
}

// A symlinked PARENT is the same attack one level up, and it is caught in
// preflight rather than at write time.
func TestPlan_SymlinkedParentCollides(t *testing.T) {
	requireSymlinks(t)
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fixture.root, ".agents/skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(fixture.root, ".agents/skills/fix")); err != nil {
		t.Fatal(err)
	}

	report, err := fixture.materialize(t)
	if !errors.Is(err, ErrCollision) {
		t.Fatalf("error = %v, want ErrCollision", err)
	}
	for _, path := range []string{
		".agents/skills/fix/SKILL.md",
		".agents/skills/fix/references/fix-heavy.md",
		".agents/skills/fix/scripts/finalize-pr.sh",
	} {
		if got := collisionReason(report, path); got != ReasonSymlink {
			t.Errorf("%s reason = %q, want %q", path, got, ReasonSymlink)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("bytes were written through the symlinked parent: %v", entries)
	}
}

// A target that exists as a directory, and a parent that exists as a file, are
// both undecidable rather than overwritable.
func TestPlan_NonFileShapesCollide(t *testing.T) {
	t.Run("target is a directory", func(t *testing.T) {
		fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
		if err := os.MkdirAll(filepath.Join(fixture.root, ".agents/skills/fix/SKILL.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		report, err := fixture.materialize(t)
		if !errors.Is(err, ErrCollision) {
			t.Fatalf("error = %v, want ErrCollision", err)
		}
		if got := collisionReason(report, ".agents/skills/fix/SKILL.md"); got != ReasonNotAFile {
			t.Fatalf("reason = %q, want %q", got, ReasonNotAFile)
		}
	})

	t.Run("parent is a file", func(t *testing.T) {
		fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
		writeWorkspaceFile(t, fixture.root, ".agents/skills/fix", "not a directory\n")

		report, err := fixture.materialize(t)
		if !errors.Is(err, ErrCollision) {
			t.Fatalf("error = %v, want ErrCollision", err)
		}
		if got := collisionReason(report, ".agents/skills/fix/SKILL.md"); got != ReasonNotADirectory {
			t.Fatalf("reason = %q, want %q", got, ReasonNotADirectory)
		}
		if got := fixture.read(t, ".agents/skills/fix"); got != "not a directory\n" {
			t.Fatalf("the conflicting file was destroyed: %q", got)
		}
	})
}

// One collision aborts EVERYTHING: not one byte of any other target moves, and
// no ownership state is written.
func TestPlan_OneCollisionProducesZeroMutations(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.write(t, ".claude/skills/fix/SKILL.md", "# mine\n")

	before := snapshotTree(t, fixture.root)
	report, err := fixture.materialize(t)
	if !errors.Is(err, ErrCollision) {
		t.Fatalf("error = %v, want ErrCollision", err)
	}
	assertSameTree(t, before, snapshotTree(t, fixture.root))

	if report.Applied {
		t.Error("an aborted run must not report itself as applied")
	}
	if len(report.Created) == 0 {
		t.Error("the report must still name what the run WOULD have created")
	}
	if _, err := os.Lstat(StatePath(fixture.root, testArtifactName)); !os.IsNotExist(err) {
		t.Errorf("an aborted run must not write ownership state: %v", err)
	}
}

// BuildPlan refuses to plan an artifact against another artifact's state; the
// ownership record is per-artifact and mixing two would authorize deletions
// nobody recorded.
func TestBuildPlan_RejectsMismatchedState(t *testing.T) {
	dir, manifestHash := buildArtifactTree(t, testArtifactName, "1.0.0", defaultWorkflowFiles())
	artifact, err := LoadArtifact(dir, testArtifactName, testEntry(manifestHash))
	if err != nil {
		t.Fatal(err)
	}
	state := StateFor(artifact)
	state.Name = "@putnami/other"

	if _, err := BuildPlan(t.TempDir(), artifact, state); err == nil {
		t.Fatal("expected a mismatched-state error")
	}
}

func TestBuildPlan_RejectsNilArtifact(t *testing.T) {
	if _, err := BuildPlan(t.TempDir(), nil, nil); err == nil {
		t.Fatal("expected an error for a nil artifact")
	}
}

// The plan is a pure read: building it twice over the same tree yields the same
// entries in the same order, and the workspace is untouched.
func TestBuildPlan_IsDeterministicAndReadOnly(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.write(t, ".claude/skills/fix/SKILL.md", "# mine\n")

	before := snapshotTree(t, fixture.root)
	_, first, err := fixture.preflight(t)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := fixture.preflight(t)
	if err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, before, snapshotTree(t, fixture.root))

	if len(first.Entries) != len(second.Entries) {
		t.Fatalf("entry counts differ: %d vs %d", len(first.Entries), len(second.Entries))
	}
	for i := range first.Entries {
		if first.Entries[i] != second.Entries[i] {
			t.Fatalf("entry %d differs: %+v vs %+v", i, first.Entries[i], second.Entries[i])
		}
	}
	if !sortedByPath(first.Entries) {
		t.Fatalf("plan entries are not sorted: %+v", first.Entries)
	}
	if !first.HasCollisions() || !first.Mutates() {
		t.Fatalf("expected a plan with both collisions and pending mutations: %+v", first)
	}
}

// Apply is the writing function, so it re-checks the collision gate itself
// rather than trusting the caller to have looked.
func TestApply_RefusesACollidedPlan(t *testing.T) {
	fixture := newFixture(t, "1.0.0", defaultWorkflowFiles())
	fixture.write(t, ".claude/skills/fix/SKILL.md", "# mine\n")

	artifact, plan, err := fixture.preflight(t)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, fixture.root)
	if err := Apply(fixture.root, artifact, plan); !errors.Is(err, ErrCollision) {
		t.Fatalf("Apply error = %v, want ErrCollision", err)
	}
	assertSameTree(t, before, snapshotTree(t, fixture.root))
}

func sortedByPath(entries []Entry) bool {
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path > entries[i].Path {
			return false
		}
	}
	return true
}

func statOf(t *testing.T, root, path string) string {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().String() + "/" + strings.TrimSpace(info.Mode().String())
}

func requireSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
}
