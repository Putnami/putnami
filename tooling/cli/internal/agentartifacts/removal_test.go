package agentartifacts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// recordInstalled writes files into the workspace and records them as managed
// by name, exactly as a successful materialization would have.
func recordInstalled(t *testing.T, root, name string, files map[string]string) *State {
	t.Helper()
	state := &State{
		Version:         StateVersion,
		Name:            name,
		ArtifactVersion: "1.0.0",
		ArchiveDigest:   lockfile.HashBytes([]byte("archive")),
		ManifestHash:    lockfile.HashBytes([]byte("manifest")),
	}
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		state.Files = append(state.Files, File{Path: path, SHA256: lockfile.HashBytes([]byte(content))})
	}
	state.Files = sortedFiles(state.Files)
	if err := WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRemovalRemovesManagedFilesAndReleasesEditedOnes(t *testing.T) {
	root := t.TempDir()
	state := recordInstalled(t, root, testArtifactName, map[string]string{
		".agents/skills/a/SKILL.md": "a\n",
		".agents/skills/b/SKILL.md": "b\n",
		".agents/skills/c/SKILL.md": "c\n",
	})
	if err := os.WriteFile(filepath.Join(root, ".agents", "skills", "b", "SKILL.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".agents", "skills", "c", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildRemovalPlan(root, state)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(plan.Select(ActionRemove)); got != 1 {
		t.Fatalf("removable = %d, want only the unedited file", got)
	}
	if got := plan.Collisions(); len(got) != 1 || got[0].Reason != ReasonModifiedRemoval {
		t.Fatalf("collisions = %+v, want the edited file as a modified removal", got)
	}

	released, err := ApplyRemoval(root, plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(released, ",") != ".agents/skills/b/SKILL.md" {
		t.Fatalf("released = %v", released)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents", "skills", "a")); !os.IsNotExist(err) {
		t.Fatal("the unedited file (and its now-empty directory) must be removed")
	}
	if data, err := os.ReadFile(filepath.Join(root, ".agents", "skills", "b", "SKILL.md")); err != nil || string(data) != "edited\n" {
		t.Fatalf("the edited file must be preserved, got %q / %v", data, err)
	}

	if err := RemoveState(root, testArtifactName); err != nil {
		t.Fatal(err)
	}
	if again, err := LoadState(root, testArtifactName); err != nil || again != nil {
		t.Fatalf("state after removal = %+v, %v; want none", again, err)
	}
	if err := RemoveState(root, testArtifactName); err != nil {
		t.Fatalf("removing an absent record must be a no-op: %v", err)
	}
}

func TestBuildRemovalPlanNeedsAState(t *testing.T) {
	if _, err := BuildRemovalPlan(t.TempDir(), nil); err == nil {
		t.Fatal("a removal with no ownership record must be refused")
	}
}

func TestListStatesReturnsEveryRecordSorted(t *testing.T) {
	root := t.TempDir()
	if states, err := ListStates(root); err != nil || len(states) != 0 {
		t.Fatalf("empty workspace: %v, %v", states, err)
	}
	recordInstalled(t, root, "@z/workflows", map[string]string{".agents/skills/z/SKILL.md": "z\n"})
	recordInstalled(t, root, "@a/workflows", map[string]string{".agents/skills/y/SKILL.md": "y\n"})
	// An interrupted staged write is not a record.
	if err := os.WriteFile(StatePath(root, "@a/workflows")+".tmp.123", []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	states, err := ListStates(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].Name != "@a/workflows" || states[1].Name != "@z/workflows" {
		t.Fatalf("states = %+v", states)
	}
}

func TestListStatesRejectsAMisfiledRecord(t *testing.T) {
	root := t.TempDir()
	recordInstalled(t, root, "@a/workflows", map[string]string{".agents/skills/y/SKILL.md": "y\n"})
	data, err := os.ReadFile(StatePath(root, "@a/workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(root, "@b/workflows"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	var corrupt *CorruptStateError
	if _, err := ListStates(root); !errors.As(err, &corrupt) || !strings.Contains(err.Error(), "not the artifact this file name belongs to") {
		t.Fatalf("error = %v, want a misfiled-record rejection", err)
	}

	root = t.TempDir()
	if err := os.MkdirAll(filepath.Dir(StatePath(root, "@x/y")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(root, "@x/y"), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListStates(root); !errors.As(err, &corrupt) || !strings.Contains(err.Error(), "records no artifact name") {
		t.Fatalf("error = %v, want a nameless-record rejection", err)
	}
}
