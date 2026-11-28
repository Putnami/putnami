package machineproc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/gitread"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// TestAHeldRunIsRefusedOnceWhatItsDiscoveryReadMovedAfterTheRead pins the
// witnesses of what a held run's discovery read (CLI ADR 0116 §6a), as the
// worker decides the next request with them: each is taken before
// discovery read its path, so a manifest written after discovery read it,
// while discovery still ran, refuses the next request, and so does an
// extension that arrives where discovery found none, whose absence is
// witnessed. The request then computes rather than being answered from
// memory, and the decision says why.
func TestAHeldRunIsRefusedOnceWhatItsDiscoveryReadMovedAfterTheRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, root := watchedTree(t, 0)
	write(t, filepath.Join(root, "putnami.workspace.json"), "{}")
	base := t.TempDir()
	outside, missing := filepath.Join(base, "a-outside"), filepath.Join(base, "b-missing")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(outside, "putnami.extension.json")
	write(t, manifest, `{"name":"@test/outside","cliContract":11}`)
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{outside: "", missing: ""}}}
	req := request{Dir: root, Args: []string{"build"}}
	memory.Lock()
	previousWatcher := memory.watcher
	memory.watcher, memory.watched[root] = w, true
	memory.Unlock()
	t.Cleanup(func() {
		memory.Lock()
		memory.watcher, memory.run = previousWatcher, runMemory{}
		delete(memory.watched, root)
		delete(memory.held, root)
		delete(memory.gitDirs, root)
		delete(memory.gitConfig, root)
		memory.Unlock()
	})
	// hold makes the worker hold a run whose engine run's discovery took
	// its witnesses with witness, and decides req against it.
	hold := func(witness func(path string) string) MemoryDecision {
		t.Helper()
		taken := extension.WitnessProbes(witness)
		discovered, err := extension.DiscoverExtensionsDetailed(root, cfg, nil)
		witnesses := taken()
		if err != nil || extension.FindExtensionByName(discovered.Extensions, "@test/outside") == nil {
			t.Fatalf("discovered %+v, %v: want the outside extension", discovered, err)
		}
		syncPoint(t, w, root)
		held := &heldRun{
			request: requestDigest(req), seq: w.Since(root, ^uint64(0)).Seq, witness: outsideWitness(root),
			runs: []heldEngineRun{{record: heldRecord{witnesses: witnesses}, plan: 0}},
		}
		memory.Lock()
		memory.held[root] = held
		memory.Unlock()
		_, d := beforeRun(req)
		return d
	}

	edited := false
	d := hold(func(path string) string {
		if path == missing && !edited {
			// Discovery read the outside manifest, and is still running.
			edited = true
			write(t, manifest, `{"name":"@test/outside","version":"2.0.0","cliContract":11}`)
		}
		return Witness(path)
	})
	if want := "a file the held run's discovery read moved: " + manifest; !edited || d.Answerable || d.Reason != want {
		t.Errorf("a manifest written after discovery read it: decided %+v, want refused: %s", d, want)
	}

	if d := hold(Witness); !d.Answerable {
		t.Fatalf("decided %+v, want answerable while nothing discovery read moved", d)
	}
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(missing, "putnami.extension.json"), `{"name":"@test/missing","cliContract":11}`)
	if _, d := beforeRun(req); d.Answerable || d.Reason != "a file the held run's discovery read moved: "+missing {
		t.Errorf("an extension arrived where discovery found none: decided %+v, want refused, naming %s", d, missing)
	}
}

// TestAGitConfigurationThatMovesWhileGitListsItIsNotTrusted pins the list of
// Git's configuration files the decision witnesses (CLI ADR 0116 §6a): it is
// made under stats taken before Git reads the files, and a file that moved
// while Git listed them leaves a witness that holds nothing, so no run is
// answered from memory under it; the next decision lists them again.
func TestAGitConfigurationThatMovesWhileGitListsItIsNotTrusted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	if out, err := exec.Command(gitread.Executable(), "-C", root, "init", "-q").CombinedOutput(); err != nil { //nolint:gosec // fixed arguments
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		memory.Lock()
		delete(memory.gitDirs, root)
		delete(memory.gitConfig, root)
		memory.Unlock()
	})
	gitconfig := filepath.Join(home, ".gitconfig")
	write(t, gitconfig, "[core]\n\tabbrev = 9\n")
	original := gitConfigListing
	t.Cleanup(func() { gitConfigListing = original })
	listings := 0
	gitConfigListing = func(root string) ([]byte, error) {
		listings++
		if listings == 2 {
			write(t, gitconfig, "[core]\n\tabbrev = 9\n[include]\n\tpath = extra.inc\n")
		}
		return original(root)
	}
	unsettled := outsideWitness(root)
	if listings != 2 || unsettled["git configuration"] == "" {
		t.Fatalf("Git listed %d times, witnessed %v: want a configuration written while Git listed it witnessed unsettled", listings, unsettled)
	}
	memory.Lock()
	kept := memory.gitConfig[root]
	memory.Unlock()
	if kept != nil {
		t.Fatal("a list made while the configuration moved was kept")
	}
	gitConfigListing = original
	if moved := movedWitness(unsettled, outsideWitness(root)); moved == "" {
		t.Fatal("a witness taken under an unsettled list held at the next decision")
	}
	paths, settled := gitConfigFiles(root)
	if !settled {
		t.Fatal("a configuration that did not move was not settled")
	}
	found := false
	for _, path := range paths {
		found = found || path == filepath.Join(home, "extra.inc")
	}
	if !found {
		t.Errorf("listed %v, want the file the configuration includes", paths)
	}
	if moved := movedWitness(outsideWitness(root), outsideWitness(root)); moved != "" {
		t.Errorf("nothing moved: moved %q", moved)
	}
}
