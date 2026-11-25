//go:build linux || darwin

package toolchain

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// placeSelfTool copies the built selftool to path, a file of its own with one
// link, and writes its configuration beside it.
func placeSelfTool(t *testing.T, path string, cfg selfToolConfig) string {
	t.Helper()
	builtSelfTool.once.Do(func() {
		dir, err := os.MkdirTemp("", "putnami-selftool-")
		if err != nil {
			builtSelfTool.err = err
			return
		}
		builtSelfTool.path = filepath.Join(dir, "selftool")
		builtSelfTool.out, builtSelfTool.err = exec.Command("go", "build", "-o", builtSelfTool.path, "./testdata/selftool").CombinedOutput()
	})
	if builtSelfTool.err != nil {
		t.Skipf("cannot build testdata/selftool: %v\n%s", builtSelfTool.err, builtSelfTool.out)
	}
	data, err := os.ReadFile(builtSelfTool.path)
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, path, string(data))
	config, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".json", config, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// manifestProbe returns the probe of the toolchain alias of the first-party
// manifest of project, a directory of this repository.
func manifestProbe(t *testing.T, project, alias string) extensionproto.RuntimeToolchainProbe {
	t.Helper()
	var manifest extensionproto.Manifest
	path := filepath.Join("..", "..", "..", "..", "..", project, "putnami.extension.json")
	if err := json.Unmarshal(mustRead(t, path), &manifest); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if manifest.Runtime == nil {
		t.Fatalf("%s declares no runtime", path)
	}
	toolchain, ok := manifest.Runtime.Toolchains[alias]
	if !ok {
		t.Fatalf("%s declares no toolchain %s", path, alias)
	}
	return toolchain.Probe
}

// probeRecords returns the records under storeRoot: answers and verdicts.
func probeRecords(t *testing.T, storeRoot string) (answers, namesOther int) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(storeRoot, toolchainProbeDir))
	for _, entry := range entries {
		var record probeRecord
		if err := json.Unmarshal(mustRead(t, filepath.Join(storeRoot, toolchainProbeDir, entry.Name())), &record); err != nil {
			t.Fatal(err)
		}
		if record.NamesOther {
			namesOther++
		} else {
			answers++
		}
	}
	return answers, namesOther
}

// realTool returns the real path of name on PATH, or skips.
func realTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not on PATH: %v", name, err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Skip(err)
	}
	return real
}

func TestAPassthroughDispatcherIsNotRecorded(t *testing.T) {
	// A compiled dispatcher execs its target with its own argv unchanged. The
	// self of the first-party manifests reads the file the kernel runs, so it
	// names the target, and the dispatcher's answer is never recorded.
	for _, tc := range []struct {
		name   string
		target func(t *testing.T) string
		probe  extensionproto.RuntimeToolchainProbe
	}{
		{"go", func(t *testing.T) string { return realTool(t, "go") }, manifestProbe(t, "go/extension", "runtimeCompiler")},
		{"bun", func(t *testing.T) string { return realTool(t, "bun") }, manifestProbe(t, "typescript/extension", "taskRuntime")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(probeAnswers.Clear)
			target := tc.target(t)
			storeRoot := t.TempDir()
			dispatcher := placeSelfTool(t, filepath.Join(t.TempDir(), "bin", tc.name), selfToolConfig{Exec: target})
			for range 2 {
				probeAnswers.Clear()
				if _, _, err := probeRuntimeToolchainVersion(context.Background(), storeRoot, false, dispatcher, tc.probe, "0", os.Environ()); err != nil {
					t.Fatalf("probe through the dispatcher: %v", err)
				}
			}
			if answers, other := probeRecords(t, storeRoot); answers != 0 || other != 1 {
				t.Fatalf("records = %d answers, %d verdicts; want the verdict that the dispatcher names another file", answers, other)
			}
		})
	}

	// An answer read from argv[0], as PHP's PHP_BINARY on Unix, names the
	// dispatcher: such a toolchain declares no self.
	t.Run("argv0", func(t *testing.T) {
		if self := manifestProbe(t, "php/extension", "php").Self; self != nil {
			t.Fatalf("the PHP manifest declares self %+v; PHP_BINARY is read from argv[0]", *self)
		}
		t.Cleanup(probeAnswers.Clear)
		storeRoot := t.TempDir()
		target := placeSelfTool(t, filepath.Join(t.TempDir(), "bin", "compiler"), selfToolConfig{Version: "1.2.3", Argv0Root: true})
		dispatcher := placeSelfTool(t, filepath.Join(t.TempDir(), "bin", "compiler"), selfToolConfig{Exec: target})
		if _, err := probeSystem(t, storeRoot, dispatcher, selfProbe); err != nil {
			t.Fatal(err)
		}
		if answers, _ := probeRecords(t, storeRoot); answers != 1 {
			t.Fatalf("an argv[0] answer through a passthrough dispatcher recorded %d answers; this test shows it records one", answers)
		}
	})
}

func TestACwdSelectedVersionThatDiffersDuringTheCheckIsNotRecorded(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	// A binary that names itself but answers a version its working directory
	// selects, as go without GOTOOLCHAIN=local under a go.mod: the version
	// that decides comes from the working directory, and the answer of an
	// empty directory differs, so it is probed on each run.
	work := t.TempDir()
	writeVersion := func(name, version string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(version+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeVersion(".version", "1.2.4")
	t.Chdir(work)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool := placeSelfTool(t, filepath.Join(root, "bin", "compiler"), selfToolConfig{Version: "1.2.3", Root: root})
	storeRoot := t.TempDir()
	if got, err := probeSystem(t, storeRoot, tool, selfProbe); err != nil || got != "1.2.4" {
		t.Fatalf("probe = %q, %v; want 1.2.4, the version the working directory selects", got, err)
	}
	writeVersion(".version", "1.2.5")
	if got, err := probeSystem(t, storeRoot, tool, selfProbe); err != nil || got != "1.2.5" {
		t.Fatalf("probe after the switch = %q, %v; want 1.2.5", got, err)
	}
	if answers, other := probeRecords(t, storeRoot); answers != 0 || other != 1 {
		t.Fatalf("records = %d answers, %d verdicts; want the verdict alone", answers, other)
	}

	// A dispatcher whose working directory selects its target answers that
	// target's version.
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second := placeSelfTool(t, filepath.Join(other, "bin", "compiler"), selfToolConfig{Version: "1.2.6", Root: other})
	if err := os.Remove(filepath.Join(work, ".version")); err != nil {
		t.Fatal(err)
	}
	writeVersion(".dispatch-target", second)
	dispatcher := placeSelfTool(t, filepath.Join(t.TempDir(), "bin", "compiler"), selfToolConfig{Exec: tool})
	if got, err := probeSystem(t, storeRoot, dispatcher, selfProbe); err != nil || got != "1.2.6" {
		t.Fatalf("dispatcher probe = %q, %v; want 1.2.6, the target of the working directory", got, err)
	}
	if answers, _ := probeRecords(t, storeRoot); answers != 0 {
		t.Fatalf("a cwd-selected dispatch recorded %d answers", answers)
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-dispatcher-whose-target-changes-is-probed-again")
}

func TestAShimAnswersTheWorkspacePin(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	// A version manager's shim walks up from the working directory for the
	// workspace's pin and falls back to a global version.
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".tool-version"), []byte("1.2.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(workspace, "project")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	shim := filepath.Join(t.TempDir(), "compiler")
	writeExecutable(t, shim, `#!/bin/sh
dir=$(pwd -P)
while [ "$dir" != "/" ]; do
  if [ -f "$dir/.tool-version" ]; then
    case "$1" in version) cat "$dir/.tool-version" ;; *) echo /pinned ;; esac
    exit 0
  fi
  dir=$(dirname "$dir")
done
case "$1" in version) echo 1.2.3 ;; *) echo /global ;; esac
`)
	storeRoot := t.TempDir()
	for range 2 {
		if got, err := probeSystem(t, storeRoot, shim, selfProbe); err != nil || got != "1.2.4" {
			t.Fatalf("shim probe = %q, %v; want the workspace pin 1.2.4", got, err)
		}
	}
	if answers, _ := probeRecords(t, storeRoot); answers != 0 {
		t.Fatalf("a shim recorded %d answers", answers)
	}
}

func TestACheckOfAMovedBinaryRecordsNothing(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool := placeSelfTool(t, filepath.Join(root, "bin", "compiler"), selfToolConfig{Version: "1.2.3", Root: root})
	identity, keep := identifyProbe(tool, nil, nil, nil, nil, selfProbe.Self)
	if !keep {
		t.Fatal("no identity")
	}
	if got := checkSelf(context.Background(), tool, *selfProbe.Self, selfProbe.Args, os.Environ(), identity, "1.2.3"); got != selfNamesExecutable {
		t.Fatalf("verdict = %v, want selfNamesExecutable", got)
	}
	// The same file, changed after its identity was taken.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(tool, later, later); err != nil {
		t.Fatal(err)
	}
	if now, _ := store.StatFileIdentity(tool); now == identity.file {
		t.Skip("the file identity did not move")
	}
	if got := checkSelf(context.Background(), tool, *selfProbe.Self, selfProbe.Args, os.Environ(), identity, "1.2.3"); got != selfUnknown {
		t.Fatalf("verdict for a binary that moved = %v, want selfUnknown", got)
	}
}

func TestARelativeSelfAnswerNamesNoFile(t *testing.T) {
	t.Cleanup(probeAnswers.Clear)
	// A relative answer is no name of the executable, even when it would
	// resolve to it from some directory.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool := placeSelfTool(t, filepath.Join(root, "bin", "compiler"), selfToolConfig{Version: "1.2.3", Root: filepath.Base(root)})
	t.Chdir(filepath.Dir(root))
	identity, _ := identifyProbe(tool, nil, nil, nil, nil, selfProbe.Self)
	if got := checkSelf(context.Background(), tool, *selfProbe.Self, selfProbe.Args, os.Environ(), identity, "1.2.3"); got != selfNamesOther {
		t.Fatalf("verdict for a relative answer = %v, want selfNamesOther", got)
	}
	if !strings.HasPrefix(tool, root) {
		t.Fatalf("tool %s is not under %s", tool, root)
	}
}
