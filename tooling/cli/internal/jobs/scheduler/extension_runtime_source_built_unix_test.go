//go:build unix

package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// sourceBuiltFixture is a source workspace: its lock records the workspace as
// the source of its CLI and pins the compiler. Its path extension tools/acme
// serves the cache provider from a runtime that runtime.prepare builds with
// that compiler, a program outside the workspace. Each prepare appends a line
// to prepares, and each compiler probe a line to probes.
type sourceBuiltFixture struct {
	root, store, prepares, probes string
}

func newSourceBuiltFixture(t *testing.T) sourceBuiltFixture {
	t.Helper()
	tools := t.TempDir()
	f := sourceBuiltFixture{
		root:     t.TempDir(),
		store:    t.TempDir(),
		prepares: filepath.Join(tools, "prepares"),
		probes:   filepath.Join(tools, "probes"),
	}
	compiler := filepath.Join(tools, "go")
	writeExecutable(t, filepath.Join(compiler, "bin", "go"),
		"#!/bin/sh\nprintf 'probe\\n' >> "+shellQuote(f.probes)+"\nprintf 'go1.26.9\\n'\n")
	t.Setenv("ACME_TEST_GOROOT", compiler)
	t.Setenv("PUTNAMI_ARTIFACT_DIR", f.store)
	// A toolchain pin without archives is lock format version 4.
	locked := lockfile.NewLockFile()
	locked.Version = lockfile.FormatVersionV4
	locked.SetCLI(lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	locked.SetToolchain("go", lockfile.LockEntry{
		Version:     "1.26.9",
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): "sha256-compiler"},
	})
	if err := lockfile.WriteLockFile(f.root, locked); err != nil {
		t.Fatal(err)
	}
	writeRuntimeFixtureWithControls(t, filepath.Join(f.root, "tools", "acme"), "@acme/cloud", "1.0.0",
		runtimeFixtureControls{counterPath: f.prepares})
	return f
}

// extension returns a new description of tools/acme, as the discovery of
// each run does.
func (f sourceBuiltFixture) extension() *extension.ExtensionDescription {
	ext := runtimeTestExtension(filepath.Join(f.root, "tools", "acme"))
	ext.Name, ext.Version, ext.RelPath = "@acme/cloud", "1.0.0", "tools/acme"
	ext.Jobs = map[string]*extension.JobDefinition{"cache-provider": {Command: "{extensionRuntime}"}}
	ext.Runtime.Toolchains = map[string]extensionproto.RuntimeToolchain{"compiler": {
		Lock: "go",
		Candidates: []extensionproto.RuntimeToolchainCandidate{{
			From: extensionproto.RuntimeToolchainCandidateEnvironment, Environment: "ACME_TEST_GOROOT", Path: "bin/go",
		}},
		Probe: extensionproto.RuntimeToolchainProbe{Args: []string{"env", "GOVERSION"}, Expect: "go{version}"},
	}}
	ext.Runtime.Prepare.Toolchains = []string{"compiler"}
	return ext
}

// prepare runs what `putnami extensions prepare` runs before the hosted run
// (PrepareSourceBuiltRuntimes): no run credential, the prepare toolchains
// resolved by a probe. It returns the prepared description.
func (f sourceBuiltFixture) prepare(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	ext := f.extension()
	if err := f.prepareExtension(ext); err != nil {
		t.Fatalf("prepare the runtime before the hosted run: %v", err)
	}
	if fileLines(t, f.prepares) != 1 || fileLines(t, f.probes) == 0 {
		t.Fatalf("the preparation ran %d prepares and %d probes, want 1 prepare and a probe",
			fileLines(t, f.prepares), fileLines(t, f.probes))
	}
	return ext
}

// prepareExtension runs PrepareSourceBuiltRuntimes on ext alone.
func (f sourceBuiltFixture) prepareExtension(ext *extension.ExtensionDescription) error {
	return PrepareSourceBuiltRuntimes(context.Background(), &workspace.Workspace{Root: f.root}, []*extension.ExtensionDescription{ext})
}

// fileLines counts the lines of path, 0 when it does not exist.
func fileLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// A hosted run of a source workspace starts the provider runtime that `putnami
// extensions prepare` admitted to the artifact store before the run. It finds
// it under the digest it computes from the tested inputs and the workspace
// lock, and runs no prepare command and no toolchain probe, so it records no
// repository code and the provider still receives the run credential.
func TestAHostedRunLoadsASourceBuiltRuntimeFromTheStoreWithoutPreparingIt(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "source-built-providers",
		"a-hosted-run-loads-a-source-built-runtime-from-the-store-without-preparing-it")
	f := newSourceBuiltFixture(t)
	prepared := f.prepare(t)
	probes := fileLines(t, f.probes)

	restore := runcredential.SetForTest("run-bearer")
	defer restore()
	hosted := f.extension()
	if !extension.FromSourceBuild(f.root, hosted) || extension.StartsRepositoryCode(f.root, hosted) {
		t.Fatal("a hosted run does not start the runtime of tools/acme from the source build")
	}
	if err := synchronizeExtensionRuntimesResolving(context.Background(), f.root, []*extension.ExtensionDescription{hosted}, nil, false); err != nil {
		t.Fatalf("a hosted run did not load the prepared runtime: %v", err)
	}
	if got := fileLines(t, f.prepares); got != 1 {
		t.Errorf("a hosted run ran the prepare command: %d prepares, want the 1 that ran before the run", got)
	}
	if got := fileLines(t, f.probes); got != probes {
		t.Errorf("a hosted run probed the compiler %d times, want none", got-probes)
	}
	if hosted.RuntimeDigest != prepared.RuntimeDigest || hosted.RuntimeExecutable != prepared.RuntimeExecutable {
		t.Errorf("a hosted run loaded %s at %s, want the prepared %s at %s",
			hosted.RuntimeDigest, hosted.RuntimeExecutable, prepared.RuntimeDigest, prepared.RuntimeExecutable)
	}
	if want := artifactstore.New(f.store).Path(prepared.RuntimeDigest); !strings.HasPrefix(hosted.RuntimeExecutable, want+string(filepath.Separator)) {
		t.Errorf("a hosted run starts %s, want a runtime under %s", hosted.RuntimeExecutable, want)
	}
	if err := runcredential.RequireCustody("the cache provider of @acme/cloud"); err != nil {
		t.Errorf("loading the prepared runtime recorded repository code: %v", err)
	}
}

// A hosted run of a source workspace whose provider runtime the store lacks,
// because an input changed after `putnami extensions prepare` ran, fails the
// launch and names the step that prepares it. It prepares nothing itself.
func TestASourceBuiltRuntimeTheStoreLacksFailsTheHostedLaunch(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "source-built-providers",
		"a-source-built-runtime-the-store-lacks-fails-the-launch")
	f := newSourceBuiltFixture(t)
	f.prepare(t)
	mustWriteRuntimeFile(t, filepath.Join(f.root, "tools", "acme", "cmd", "source"), "changed after the preparation", 0o644)
	probes := fileLines(t, f.probes)

	restore := runcredential.SetForTest("run-bearer")
	defer restore()
	hosted := f.extension()
	err := synchronizeExtensionRuntimesResolving(context.Background(), f.root, []*extension.ExtensionDescription{hosted}, nil, false)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeExecutableMissing {
		t.Fatalf("a hosted run with no prepared runtime returned %v, want %s", err, extensionproto.FailureRuntimeExecutableMissing)
	}
	if !strings.Contains(err.Error(), "./putnamiw --print-engine") {
		t.Errorf("the failure %q does not name the step that prepares the runtime", err)
	}
	if got := fileLines(t, f.prepares); got != 1 {
		t.Errorf("a hosted run ran the prepare command: %d prepares, want the 1 that ran before the run", got)
	}
	if got := fileLines(t, f.probes); got != probes {
		t.Errorf("a hosted run probed the compiler %d times, want none", got-probes)
	}
	if hosted.RuntimeExecutable != "" {
		t.Errorf("a failed launch set the runtime %s", hosted.RuntimeExecutable)
	}
}

// `putnami extensions prepare` fails when the hosted run would compute another
// digest than the preparation gave. An optional prepare toolchain that the
// lock pins counts as available for the hosted run, so a preparation where no
// candidate matched it admits the runtime under a digest the hosted run never
// looks for.
func TestPreparingASourceBuiltRuntimeFailsWhenAHostedRunWouldMissIt(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "source-built-providers", "extensions-prepare-prepares-the-source-built-runtimes")
	f := newSourceBuiltFixture(t)
	t.Setenv("ACME_TEST_GOROOT", filepath.Join(t.TempDir(), "no-go"))
	ext := f.extension()
	compiler := ext.Runtime.Toolchains["compiler"]
	compiler.Optional = true
	ext.Runtime.Toolchains["compiler"] = compiler

	err := f.prepareExtension(ext)
	if err == nil || !strings.Contains(err.Error(), "a hosted run looks for") {
		t.Fatalf("a preparation a hosted run would miss returned %v, want the digest mismatch", err)
	}
	if got := fileLines(t, f.prepares); got != 1 {
		t.Errorf("the preparation ran %d prepares, want 1", got)
	}
}

// `putnami extensions prepare` refuses a provider command that resolves a
// runtime toolchain, before it prepares anything: a hosted run records
// repository code before it probes a toolchain, so that provider would receive
// no run credential.
func TestPreparingASourceBuiltRuntimeRefusesAProviderThatProbesAToolchain(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "source-built-providers", "extensions-prepare-prepares-the-source-built-runtimes")
	f := newSourceBuiltFixture(t)
	ext := f.extension()
	ext.Jobs["cache-provider"].Toolchains = []string{"compiler"}

	err := f.prepareExtension(ext)
	if err == nil || !strings.Contains(err.Error(), `provider command "cache-provider" resolves the runtime toolchains compiler`) {
		t.Fatalf("a provider that probes a toolchain returned %v, want a refusal", err)
	}
	if got := fileLines(t, f.prepares); got != 0 {
		t.Errorf("the refused preparation ran %d prepares, want none", got)
	}
}
