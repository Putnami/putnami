//go:build unix

package main

import (
	"bytes"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/putnamihome"
)

// requireBun returns the bun of the Bun release this repository's lock pins,
// the release the CLI runs TypeScript jobs with, and puts the directory of
// that program first on PATH for the test, so code under test that finds bun
// on PATH runs the same release. The program is the first candidate of the
// task runtime the extension manifest declares that answers its probe with
// that release, through symbolic links. The test is skipped when the machine
// holds none.
func requireBun(t *testing.T) string {
	t.Helper()
	root := repositoryRoot(t)
	taskRuntime := manifestTaskRuntime(t)
	data, err := os.ReadFile(filepath.Join(root, "putnami.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	lock, diagnostics := wsproto.ParseLock(data)
	if len(diagnostics) > 0 {
		t.Fatalf("parse putnami.lock.json: %v", diagnostics)
	}
	pin := lock.Toolchains[taskRuntime.Lock]
	if pin.Version == "" {
		t.Fatalf("putnami.lock.json pins no %s release", taskRuntime.Lock)
	}
	for _, candidate := range taskRuntimeCandidates(t, root, taskRuntime, pin) {
		bun, err := filepath.EvalSymlinks(candidate)
		if err != nil || !answersProbe(bun, taskRuntime.Probe, pin.Version) {
			continue
		}
		t.Setenv("PATH", filepath.Dir(bun)+string(os.PathListSeparator)+os.Getenv("PATH"))
		return bun
	}
	t.Skipf("no Bun %s, the release putnami.lock.json pins, on this machine: run `./putnamiw install`", pin.Version)
	return ""
}

// manifestTaskRuntime returns the taskRuntime toolchain of the extension
// manifest.
func manifestTaskRuntime(t *testing.T) extensionproto.RuntimeToolchain {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diagnostics := extensionproto.ParseManifest(data)
	if len(diagnostics) > 0 {
		t.Fatalf("parse putnami.extension.json: %v", diagnostics)
	}
	if manifest.Runtime == nil {
		t.Fatal("putnami.extension.json declares no runtime")
	}
	taskRuntime, ok := manifest.Runtime.Toolchains["taskRuntime"]
	if !ok {
		t.Fatal("putnami.extension.json declares no taskRuntime toolchain")
	}
	return taskRuntime
}

// taskRuntimeCandidates expands the candidates of taskRuntime, in their
// order, to the programs they name for the release pin in the workspace at
// root: a path candidate on PATH, an environment candidate under its
// variable, a home candidate under the user's home directory, a putnami-home
// candidate under the Putnami home, and a provisioned candidate at the
// executable of this platform's archive in the release directory under the
// Putnami home. A path candidate that PATH does not hold, an environment
// candidate whose variable is empty, a home candidate without a home
// directory and a provisioned candidate without an archive for this platform
// are left out; the other paths may name no file.
func taskRuntimeCandidates(t *testing.T, root string, taskRuntime extensionproto.RuntimeToolchain, pin wsproto.LockToolchainEntry) []string {
	t.Helper()
	toolchains := putnamihome.ToolchainRoot(os.Getenv, root, taskRuntime.Lock)
	putnamiHome := filepath.Dir(filepath.Dir(toolchains))
	var programs []string
	for _, candidate := range taskRuntime.Candidates {
		rel := filepath.FromSlash(strings.ReplaceAll(candidate.Path, extensionproto.RuntimeToolchainVersionToken, pin.Version))
		switch candidate.From {
		case extensionproto.RuntimeToolchainCandidatePath:
			if program, err := stdexec.LookPath(rel); err == nil {
				programs = append(programs, program)
			}
		case extensionproto.RuntimeToolchainCandidateEnvironment:
			if dir := strings.TrimSpace(os.Getenv(candidate.Environment)); dir != "" {
				programs = append(programs, filepath.Join(dir, rel))
			}
		case extensionproto.RuntimeToolchainCandidateHome:
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				programs = append(programs, filepath.Join(home, rel))
			}
		case extensionproto.RuntimeToolchainCandidatePutnamiHome:
			programs = append(programs, filepath.Join(putnamiHome, rel))
		case extensionproto.RuntimeToolchainCandidateProvisioned:
			if archive, ok := pin.Archives[runtime.GOOS+"/"+runtime.GOARCH]; ok && archive.Executable != "" {
				release := filepath.Join(toolchains, taskRuntime.Lock+"-"+pin.Version)
				programs = append(programs, filepath.Join(release, filepath.FromSlash(archive.Executable)))
			}
		default:
			t.Fatalf("the taskRuntime toolchain names a candidate from %q, which requireBun does not expand", candidate.From)
		}
	}
	return programs
}

// answersProbe reports whether program answers what probe expects of
// version: its standard output, or its standard error when it writes nothing
// to its standard output, trimmed. It runs with the arguments of probe, with
// the variables probe unsets removed, the variables it sets added with
// {version} expanded to version, and its own directory first on PATH.
func answersProbe(program string, probe extensionproto.RuntimeToolchainProbe, version string) bool {
	cmd := stdexec.Command(program, probe.Args...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		_, set := probe.Environment[name]
		return set || name == "PATH" || slices.Contains(probe.Unset, name)
	})
	for name, value := range probe.Environment {
		cmd.Env = append(cmd.Env, name+"="+strings.ReplaceAll(value, extensionproto.RuntimeToolchainVersionToken, version))
	}
	cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(program)+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return false
	}
	answer := strings.TrimSpace(stdout.String())
	if answer == "" {
		answer = strings.TrimSpace(stderr.String())
	}
	return answer == strings.ReplaceAll(probe.Expect, extensionproto.RuntimeToolchainVersionToken, version)
}
