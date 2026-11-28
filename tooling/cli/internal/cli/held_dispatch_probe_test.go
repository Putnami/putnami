package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/machineproc"
)

// TestADispatchIsHeldOnlyUnderWitnessesTakenBeforeDiscoveryRead pins the
// witnesses of a held dispatch (CLI ADR 0116 §6a): those discovery took
// before it read each path, absent ones included. A manifest written after
// discovery read it moves its witness, an extension that arrives where
// discovery found none moves the witness of that absence, and a dispatch
// whose discovered manifest has no such witness is not held.
func TestADispatchIsHeldOnlyUnderWitnessesTakenBeforeDiscoveryRead(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("a dispatch is held only where the machine process runs")
	}
	base := t.TempDir()
	outside, missing := filepath.Join(base, "a-outside"), filepath.Join(base, "b-missing")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(outside, "putnami.extension.json")
	if err := os.WriteFile(manifest, []byte(`{"name":"@test/outside","cliContract":11}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Name: "held", Extensions: wsproto.ExtensionsConfig{List: map[string]string{outside: "", missing: ""}}}
	ws := t.TempDir()
	taken := extension.WitnessProbes(machineproc.Witness)
	extensions, err := extension.DiscoverExtensions(ws, cfg, nil)
	probes := taken()
	if err != nil || extension.FindExtensionByName(extensions, "@test/outside") == nil {
		t.Fatalf("discovered %v, %v: want the outside extension", extensions, err)
	}
	loaded := loadedDispatch(ws, []string{"build"}, nil, cfg, extensions, nil, probes)
	if loaded == nil {
		t.Fatal("a dispatch whose every discovered manifest was witnessed before the read was not held")
	}
	// Tests running alongside discover too: only this test's paths are
	// looked at.
	holds := func(path string) bool {
		witness, ok := loaded.Witnesses()[path]
		return ok && witness == machineproc.Witness(path)
	}
	arrived := filepath.Join(missing, "putnami.extension.json")
	for _, path := range []string{outside, manifest, missing, arrived} {
		if !holds(path) {
			t.Fatalf("%s: witnessed %q, want its stat taken before discovery read it", path, loaded.Witnesses()[path])
		}
	}
	if err := os.WriteFile(manifest, []byte(`{"name":"@test/outside","version":"2.0.0","cliContract":11}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if holds(manifest) {
		t.Error("a manifest written after discovery read it left its witness holding")
	}
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arrived, []byte(`{"name":"@test/missing","cliContract":11}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if holds(missing) || holds(arrived) {
		t.Error("an extension that arrived where discovery found none left the witness of that absence holding")
	}

	if loadedDispatch(ws, []string{"build"}, nil, cfg, extensions, nil, nil) != nil {
		t.Fatal("a dispatch whose discovered manifest no witness taken before the read covers was held")
	}
}
