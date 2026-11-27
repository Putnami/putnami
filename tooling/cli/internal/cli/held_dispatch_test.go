package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fileindex"
	"go.putnami.dev/tooling/cli/internal/machineproc"
)

// TestADispatchIsHeldWithTheWitnessesOfTheManifestsItRead pins what App.Run
// leaves a machine worker (CLI ADR 0116 §6a): what it loaded, for its
// workspace and arguments, with the witness of each extension manifest its
// discovery read, taken when it read them; a later run takes it only while
// every one of them holds.
func TestADispatchIsHeldWithTheWitnessesOfTheManifestsItRead(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("a dispatch is held only where the machine process runs")
	}
	outside := t.TempDir()
	manifest := filepath.Join(outside, "putnami.extension.json")
	if err := os.WriteFile(manifest, []byte(`{"name":"@test/outside"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Name: "held"}
	extensions := []*extension.ExtensionDescription{{Name: "@test/outside", Path: outside}, nil, {Name: "@test/pathless"}}
	args := []string{"lint,build", "--all"}
	index := fileindex.OpenReadOnly(filepath.Join(t.TempDir(), "file-index"), "")
	loaded := loadedDispatch("/ws", args, index, cfg, extensions, map[string]bool{"cloud": true})
	args[0] = "changed"
	if loaded.root != "/ws" || loaded.args[0] != "lint,build" || loaded.index != index || loaded.cfg != cfg || len(loaded.extensions) != 3 || !loaded.groups["cloud"] {
		t.Fatalf("loaded %+v, want what App.Run loaded, with its own copy of the arguments", loaded)
	}
	if len(loaded.manifests) != 1 || loaded.manifests[manifest] == "" {
		t.Fatalf("witnessed %v, want the one manifest discovery read", loaded.manifests)
	}
	if moved := loaded.movedManifest(); moved != "" {
		t.Fatalf("nothing changed: moved %q", moved)
	}
	if err := os.WriteFile(manifest, []byte(`{"name":"@test/outside","version":"2.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if moved := loaded.movedManifest(); moved != manifest {
		t.Fatalf("a rewritten manifest: moved %q, want %q", moved, manifest)
	}
	if takeHeldDispatch("", args) != nil {
		t.Fatal("a run outside a workspace took a held dispatch")
	}
}

// TestMachineStatusNamesWhatAnAnsweredRunHeld pins the human status line's
// list of what the last answered run took from the held run.
func TestMachineStatusNamesWhatAnAnsweredRunHeld(t *testing.T) {
	t.Parallel()
	if got := heldSteps(&machineproc.MemoryDecision{DispatchHeld: true, VersionsHeld: true, FinalizersHeld: true}); got != "dispatch, version snapshot, finalizers" {
		t.Errorf("all held: %q", got)
	}
	if got := heldSteps(&machineproc.MemoryDecision{FinalizersHeld: true}); got != "finalizers" {
		t.Errorf("finalizers held: %q", got)
	}
	if got := heldSteps(&machineproc.MemoryDecision{}); got != "" {
		t.Errorf("none held: %q", got)
	}
}
