package cachekey

import (
	"os"
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// entryRuntime is a synchronized workspace-local runtime whose prepare builds
// a main executable and a codegen entry from cmd/**, which it declares
// compiled, and reads config/** at run time.
type entryRuntime struct {
	ws      *workspace.Workspace
	extRoot string
	runtime string // the prepared runtime root
}

func newEntryRuntime(t *testing.T) *entryRuntime {
	t.Helper()
	ws := makeExecutorTestWorkspace(t)
	r := &entryRuntime{ws: ws, extRoot: filepath.Join(ws.Root, "go", "extension"), runtime: t.TempDir()}
	mustWriteRuntimeFile(t, filepath.Join(r.extRoot, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(r.extRoot, "cmd", "main.go"), "package main\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(r.extRoot, "config", "lint.yml"), "a: 1\n", 0o644)
	r.install(t, "compiled/putnami-go", "main v1")
	r.install(t, "compiled/putnami-go-codegen", "codegen v1")
	return r
}

func (r *entryRuntime) install(t *testing.T, rel, content string) {
	t.Helper()
	mustWriteRuntimeFile(t, filepath.Join(r.runtime, filepath.FromSlash(rel)), content, 0o755)
}

// job is a task of the extension that names entry, "" for the main
// executable, with compiled inputs declared or not.
func (r *entryRuntime) job(entry string, compiled bool) *jobmodel.ScheduledJob {
	definition := &extension.RuntimeDefinition{
		Executable: "compiled/putnami-go",
		Entries:    map[string]extension.RuntimeEntry{"codegen": {Executable: "compiled/putnami-go-codegen"}},
		Prepare: &extension.RuntimePrepare{
			Command: "{extensionRoot}/bin/prepare",
			Args:    []string{"--output", "{runtimeOutput}"},
			Inputs:  []string{"bin/prepare", "cmd/**", "config/**"},
		},
	}
	if compiled {
		definition.CompiledInputs = []string{"cmd/**"}
	}
	return &jobmodel.ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@putnami/go", Version: "1.0.0", Path: r.extRoot, RelPath: "go/extension",
			LocalSource:       true,
			Runtime:           definition,
			RuntimeExecutable: filepath.Join(r.runtime, "compiled", "putnami-go"),
			RuntimeDigest:     "synchronized",
		},
		JobDef: &extension.JobDefinition{
			Name: "build", ExtensionName: "@putnami/go", Cache: true,
			Command: "{extensionRuntime}", Args: []string{"build"}, RuntimeEntry: entry,
		},
	}
}

// digest is the implementation digest of a task naming entry, computed by a
// fresh run (a cache manager of its own), as a later run would.
func (r *entryRuntime) digest(t *testing.T, entry string) string {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	digest, err := extensionImplementationDigest(r.ws, r.job(entry, true), cache)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || digest == "synchronized" {
		t.Fatalf("digest = %q, want the entry digest", digest)
	}
	return digest
}

// The guard: a change to the executable an entry's tasks run re-keys them,
// and only them.
func TestRuntimeEntry_AChangedExecutableReKeysItsTasks(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-changed-entry-executable-re-keys-its-tasks")
	r := newEntryRuntime(t)
	main, codegen := r.digest(t, ""), r.digest(t, "codegen")
	if main == codegen {
		t.Fatal("the main executable and the codegen entry share one digest")
	}

	r.install(t, "compiled/putnami-go-codegen", "codegen v2")
	if got := r.digest(t, "codegen"); got == codegen {
		t.Error("a changed codegen executable kept the key of a task that runs it")
	}
	if got := r.digest(t, ""); got != main {
		t.Error("a changed codegen executable moved the key of a task that runs the main executable")
	}

	r.install(t, "compiled/putnami-go", "main v2")
	if got := r.digest(t, ""); got == main {
		t.Error("a changed main executable kept the key of a task that runs it")
	}
}

// A compiled input that rebuilds the same executables re-keys nothing: the
// task runs the same bytes.
func TestRuntimeEntry_ACompiledEditThatBuildsTheSameBytesKeepsTheKeys(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"an-entry-another-edit-leaves-unchanged-keeps-its-keys")
	r := newEntryRuntime(t)
	main, codegen := r.digest(t, ""), r.digest(t, "codegen")
	mustWriteRuntimeFile(t, filepath.Join(r.extRoot, "cmd", "main.go"), "package main\n\n// A comment.\n", 0o644)
	if r.digest(t, "") != main || r.digest(t, "codegen") != codegen {
		t.Error("an edit of a compiled input moved a key although no executable changed")
	}
}

// A prepare input that is not compiled reaches every task at run time, so it
// re-keys them all, whatever entry they run.
func TestRuntimeEntry_AnUncompiledInputReKeysEveryTask(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"an-uncompiled-runtime-input-re-keys-every-task")
	r := newEntryRuntime(t)
	main, codegen := r.digest(t, ""), r.digest(t, "codegen")
	mustWriteRuntimeFile(t, filepath.Join(r.extRoot, "config", "lint.yml"), "a: 2\n", 0o644)
	if r.digest(t, "") == main || r.digest(t, "codegen") == codegen {
		t.Error("an uncompiled input kept the key of a task")
	}
}

// A hook that runs the main executable can affect any task, so a task of
// another entry folds the main executable too.
func TestRuntimeEntry_AHookThatRunsTheRuntimeKeysEveryEntryOnIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-changed-entry-executable-re-keys-its-tasks")
	r := newEntryRuntime(t)
	job := r.job("codegen", true)
	job.Extension.Hooks = &extension.ManifestHooks{PreBuild: &extension.HookDefinition{Command: "{extensionRuntime}", Args: []string{"hook"}}}
	digest := func() string {
		cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
		got, err := extensionImplementationDigest(r.ws, job, cache)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := digest()
	r.install(t, "compiled/putnami-go", "main v2")
	if digest() == before {
		t.Error("with a hook that runs the main executable, a changed main executable kept a codegen task's key")
	}
}

// Without compiled inputs the runtime keeps its one identity, the synchronized
// digest, and a task keyed before synchronization keys on the whole runtime.
func TestRuntimeEntry_WithoutCompiledInputsOrSynchronizationTheRuntimeDigestStays(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-digest-replaces-the-version-in-the-key")
	r := newEntryRuntime(t)
	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	if digest, err := extensionImplementationDigest(r.ws, r.job("codegen", false), cache); err != nil || digest != "synchronized" {
		t.Errorf("without compiled inputs = %q, %v; want the synchronized digest", digest, err)
	}
	unsynchronized := r.job("codegen", true)
	unsynchronized.Extension.RuntimeExecutable = ""
	if digest, err := extensionImplementationDigest(r.ws, unsynchronized, cache); err != nil || digest != "synchronized" {
		t.Errorf("before synchronization = %q, %v; want the synchronized digest", digest, err)
	}
}

// A missing entry executable is an error, never a key that leaves it out.
func TestRuntimeEntry_AMissingEntryExecutableFailsTheKey(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-changed-entry-executable-re-keys-its-tasks")
	r := newEntryRuntime(t)
	if err := os.Remove(filepath.Join(r.runtime, "compiled", "putnami-go-codegen")); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	if _, err := extensionImplementationDigest(r.ws, r.job("codegen", true), cache); err == nil {
		t.Fatal("a missing codegen executable gave a key")
	}
	path, err := RuntimeEntryExecutable(r.job("codegen", true).Extension, "codegen")
	if err != nil || path != filepath.Join(r.runtime, "compiled", "putnami-go-codegen") {
		t.Fatalf("RuntimeEntryExecutable = %q, %v", path, err)
	}
}

// On a platform that names executables with a suffix, the main executable's
// resolved path carries it, and an entry takes the same suffix.
func TestRuntimeEntry_AnEntryTakesTheMainExecutablesSuffix(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-task-runs-its-runtime-entry")
	ext := &extension.ExtensionDescription{
		Name: "@putnami/go",
		Runtime: &extension.RuntimeDefinition{
			Executable: "compiled/putnami-go",
			Entries:    map[string]extension.RuntimeEntry{"codegen": {Executable: "compiled/putnami-go-codegen"}},
		},
		RuntimeExecutable: filepath.Join("store", "runtime", "compiled", "putnami-go.exe"),
	}
	got, err := RuntimeEntryExecutable(ext, "codegen")
	if err != nil || got != filepath.Join("store", "runtime", "compiled", "putnami-go-codegen.exe") {
		t.Fatalf("entry = %q, %v", got, err)
	}
	ext.RuntimeExecutable = filepath.Join("store", "runtime", "bin", "other")
	if _, err := RuntimeEntryExecutable(ext, "codegen"); err == nil {
		t.Fatal("a main executable off its declared path resolved an entry")
	}
}
