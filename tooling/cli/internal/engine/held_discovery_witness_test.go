package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// statHeldRuns is a worker's memory whose witness is the path's stat, and
// which calls witnessing with each path before it takes its witness.
type statHeldRuns struct {
	fakeHeldRuns
	witnessing func(path string)
}

func (s *statHeldRuns) Witness(path string) string {
	if s.witnessing != nil {
		s.witnessing(path)
	}
	st, ok := fileindex.LstatAny(path)
	if !ok {
		return "absent"
	}
	return fmt.Sprint(st)
}

// writeManifest writes the manifest of the extension name in dir.
func writeManifest(t *testing.T, dir, name, version string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, extensionproto.ManifestFilename)
	data := fmt.Sprintf(`{"name":%q,"version":%q,"cliContract":11}`, name, version)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestADiscoveryWitnessIsTakenBeforeTheRead pins when a run that may be held
// witnesses what its discovery read (CLI ADR 0116 §6a): before the read. A
// manifest written once discovery read it, while discovery still ran, moves
// the witness the run holds, and an extension the configuration names that
// discovery found nowhere is witnessed absent where it looked, so its arrival
// moves that witness: in either case the worker offers nothing of the held
// run to the next request.
func TestADiscoveryWitnessIsTakenBeforeTheRead(t *testing.T) {
	_, req := heldRunFixture(t)
	base := t.TempDir()
	outside, missing := filepath.Join(base, "a-outside"), filepath.Join(base, "b-missing")
	manifest := writeManifest(t, outside, "@test/outside", "1.0.0")
	arrived := filepath.Join(missing, extensionproto.ManifestFilename)
	req.Config.Extensions.List = map[string]string{outside: "", missing: ""}
	memory := &statHeldRuns{}
	// Discovery probes the configured paths in order: the edit lands after
	// it read the outside manifest and before it ended.
	memory.witnessing = func(path string) {
		if path == missing {
			memory.witnessing = nil
			writeManifest(t, outside, "@test/outside", "2.0.0-edited")
		}
	}
	serveHeldRuns(t, memory)
	req.hold = &heldRun{}
	if _, discovered, code := loadWorkspaceAndExtensions(&req); code != ExitSuccess || extension.FindExtensionByName(discovered.Extensions, "@test/outside") == nil {
		t.Fatalf("discovery exited %d, want the outside extension loaded", code)
	}
	if memory.witnessing != nil {
		t.Fatal("discovery did not probe the missing extension after the outside one")
	}
	if req.hold == nil {
		t.Fatal("a run whose discovery read only witnessed paths was not held")
	}
	held := req.hold.Witnesses()
	if w, ok := held[manifest]; !ok || w == memory.Witness(manifest) {
		t.Errorf("the outside manifest is witnessed %q, the edited file is %q: the witness was taken after the read", w, memory.Witness(manifest))
	}
	if w, ok := held[arrived]; !ok || w != "absent" {
		t.Fatalf("the missing extension's manifest is witnessed %q (%v), want its absence", w, ok)
	}
	writeManifest(t, missing, "@test/missing", "1.0.0")
	if held[arrived] == memory.Witness(arrived) {
		t.Error("an extension that arrived where discovery found none left its witness holding")
	}
}

// A run whose discovery loaded an extension no witness taken before the
// read covers is not held: a stat taken now would vouch for a read it did not
// see.
func TestADiscoveredExtensionWithNoWitnessIsNotHeld(t *testing.T) {
	root, _ := heldRunFixture(t)
	serveHeldRuns(t, &statHeldRuns{})
	ext := filepath.Join(root, "extension")
	discovered := &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{{Name: "@test/held", Path: ext}}}
	req := &Request{WorkspaceRoot: root, hold: &heldRun{}}
	req.witnessDiscovery(discovered, map[string]string{ext: "dir"})
	if req.hold != nil {
		t.Fatal("a run whose discovered manifest has no witness was held")
	}
	req = &Request{WorkspaceRoot: root, hold: &heldRun{}}
	manifest := filepath.Join(ext, extensionproto.ManifestFilename)
	req.witnessDiscovery(discovered, map[string]string{ext: "dir", manifest: "manifest", filepath.Join(root, "absent"): "absent"})
	if req.hold == nil || req.hold.witnesses[manifest] != "manifest" || req.hold.witnesses[filepath.Join(root, "absent")] != "absent" {
		t.Fatalf("held %+v, want every witness discovery took", req.hold)
	}
}
