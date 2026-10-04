package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const (
	canaryVersion = "0.3.1-20261003152506-4da6833"
	nextVersion   = "0.3.1-20261004094924-a389c95"
)

// writeInstalledExtension lays out the installed tree of one extension build
// stamped with version, the way a packaged archive extracts: a manifest that
// carries the version, a platform executable, a configuration file, a package
// file with a version of its own, and agent content whose manifest carries the
// version and whose digest the extension manifest binds. indent varies the
// manifests' layout, which a build's re-encoding is free to change.
func writeInstalledExtension(t *testing.T, root, version, indent string) {
	t.Helper()
	agentManifest := fmt.Sprintf("{\n%s\"name\": \"@putnami/go\",\n%s\"version\": %q,\n%s\"files\": []\n}\n",
		indent, indent, version, indent)
	sum := sha256.Sum256([]byte(agentManifest))
	manifest := fmt.Sprintf("{\n%s\"name\": \"@putnami/go\",\n%s\"version\": %q,\n%s\"runtime\": {\"executable\": \"compiled/putnami-go\"},\n%s\"agentContent\": {\"path\": \"content\", \"manifestSha256\": %q}\n}\n",
		indent, indent, version, indent, indent, hex.EncodeToString(sum[:]))
	writeTestFile(t, filepath.Join(root, "putnami.extension.json"), manifest)
	writeTestFile(t, filepath.Join(root, "package.json"), "{\"name\": \"@putnami/go\", \"version\": \"1.0.0\"}\n")
	writeTestFile(t, filepath.Join(root, "content", "putnami.agent-artifact.json"), agentManifest)
	writeTestFile(t, filepath.Join(root, "content", ".agents", "skills", "go", "SKILL.md"), "# Go\n")
	writeTestFile(t, filepath.Join(root, "config", "defaults.json"), "{\"race\": false}\n")
	writeTestFile(t, filepath.Join(root, "compiled", "putnami-go"), "runtime bytes")
	if err := os.Chmod(filepath.Join(root, "compiled", "putnami-go"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustTreeDigest(t *testing.T, root string) string {
	t.Helper()
	digest, err := installedTreeDigest(root, false)
	if err != nil {
		t.Fatalf("installedTreeDigest(%s): %v", root, err)
	}
	return digest
}

// Two builds of one implementation differ in the version they stamp into the
// extension manifest and the agent-content manifest (and so in the digest that
// binds the latter), and in nothing else. They have one identity.
func TestInstalledTreeDigestIgnoresVersionMetadata(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"an-installed-tree-digest-ignores-version-metadata")
	canary := t.TempDir()
	next := t.TempDir()
	writeInstalledExtension(t, canary, canaryVersion, "  ")
	writeInstalledExtension(t, next, nextVersion, "\t")

	if a, b := mustTreeDigest(t, canary), mustTreeDigest(t, next); a != b {
		t.Fatalf("version-only builds digest differently: %s vs %s", a, b)
	}
}

// Every byte the extension runs or reads stays in the identity: the runtime,
// a configuration file, the executable bit, a version in a file the extension
// protocol does not define, any other manifest member, the agent content, a
// symbolic link's target, and the tree's membership.
func TestInstalledTreeDigestFollowsTheImplementation(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"an-installed-tree-digest-follows-the-implementation")
	base := t.TempDir()
	writeInstalledExtension(t, base, canaryVersion, "  ")
	baseline := mustTreeDigest(t, base)

	for name, mutate := range map[string]func(t *testing.T, root string){
		"runtime bytes": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "compiled", "putnami-go"), "other runtime bytes")
			if err := os.Chmod(filepath.Join(root, "compiled", "putnami-go"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"configuration file": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "config", "defaults.json"), "{\"race\": true}\n")
		},
		"executable bit": func(t *testing.T, root string) {
			if runtime.GOOS == "windows" {
				t.Skip("Windows files carry no executable bit")
			}
			if err := os.Chmod(filepath.Join(root, "compiled", "putnami-go"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		// Only the extension protocol's manifests are read without their
		// version: a version in any other file is the extension's content.
		"a version outside the extension manifests": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "package.json"), "{\"name\": \"@putnami/go\", \"version\": \"1.0.1\"}\n")
		},
		"another manifest member": func(t *testing.T, root string) {
			path := filepath.Join(root, "putnami.extension.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, path, strings.Replace(string(data), "compiled/putnami-go", "compiled/putnami-go2", 1))
		},
		"agent content": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "content", ".agents", "skills", "go", "SKILL.md"), "# Go, revised\n")
		},
		"an added file": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "templates", "app", "main.go"), "package main\n")
		},
		"a removed file": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "config", "defaults.json")); err != nil {
				t.Fatal(err)
			}
		},
		"a symbolic link": func(t *testing.T, root string) {
			if err := os.Symlink("compiled/putnami-go", filepath.Join(root, "putnami-go")); err != nil {
				t.Skipf("symbolic links unavailable: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeInstalledExtension(t, root, canaryVersion, "  ")
			if got := mustTreeDigest(t, root); got != baseline {
				t.Fatalf("an identical tree digests differently: %s vs %s", got, baseline)
			}
			mutate(t, root)
			if got := mustTreeDigest(t, root); got == baseline {
				t.Errorf("%s did not move the installed tree digest", name)
			}
		})
	}
}

// A manifest that is not one JSON object is hashed raw: its version is not
// removed, and no raw file shares the identity of a canonical one.
func TestInstalledTreeDigestHashesAnUnreadableManifestRaw(t *testing.T) {
	digestOf := func(manifest string) string {
		t.Helper()
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "putnami.extension.json"), manifest)
		return mustTreeDigest(t, root)
	}
	if digestOf(`{"version": "1"} trailing`) == digestOf(`{"version": "2"} trailing`) {
		t.Error("a manifest with trailing data lost its version")
	}
	if digestOf(`["version"]`) == digestOf(`["other"]`) {
		t.Error("a non-object manifest was not hashed raw")
	}
	if digestOf(`{}`) == digestOf("{\"a\":1}") {
		t.Error("two different manifests share a digest")
	}
	// A valid manifest is read without its version, whatever its layout.
	if digestOf(`{"version": "1", "a": 1}`) != digestOf(`{"a":1}`) {
		t.Error("a valid manifest without its version is not its canonical form")
	}
	// Bytes that are not UTF-8 are hashed raw, and never as the canonical
	// form of the object they almost hold.
	if digestOf(`{"a":1}`+"\xff") == digestOf(`{"a":1}`) {
		t.Error("a manifest that is not UTF-8 shares the digest of its canonical neighbor")
	}
}

func installedExtensionJob(extRoot, version string) *ScheduledJob {
	return &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@putnami/go", Version: version, Path: extRoot,
			Runtime: &extension.RuntimeDefinition{Executable: "compiled/putnami-go"},
		},
		JobDef: &extension.JobDefinition{
			Name: "build", ExtensionName: "@putnami/go", Cache: true,
			Command: "{extensionRuntime}", Args: []string{"build"},
		},
	}
}

func installedJobKey(t *testing.T, ws *workspace.Workspace, job *ScheduledJob) string {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("computeJobCacheHash: %v", err)
	}
	return hash
}

// A task of an installed extension keeps its key across builds that differ in
// their version alone, and moves when the installed implementation does.
func TestInstalledExtensionTreeKeysItsTasks(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-version-only-build-keeps-the-key")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	ws := makeExecutorTestWorkspace(t)
	canary := filepath.Join(t.TempDir(), "canary")
	next := filepath.Join(t.TempDir(), "next")
	writeInstalledExtension(t, canary, canaryVersion, "  ")
	writeInstalledExtension(t, next, nextVersion, "\t")

	before := installedJobKey(t, ws, installedExtensionJob(canary, canaryVersion))
	if after := installedJobKey(t, ws, installedExtensionJob(next, nextVersion)); after != before {
		t.Fatalf("a version-only build moved the task key: %s -> %s", before, after)
	}
	writeTestFile(t, filepath.Join(next, "compiled", "putnami-go"), "other runtime bytes")
	if after := installedJobKey(t, ws, installedExtensionJob(next, nextVersion)); after == before {
		t.Fatal("a changed installed runtime kept the task key")
	}
}

// An installed extension whose directory is gone has no tree to digest; its
// version stays its identity.
func TestInstalledExtensionWithoutATreeKeepsItsVersionIdentity(t *testing.T) {
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	ws := makeExecutorTestWorkspace(t)
	missing := filepath.Join(t.TempDir(), "missing")
	digest, err := extensionImplementationDigest(ws, installedExtensionJob(missing, canaryVersion), nil)
	if err != nil || digest != "" {
		t.Fatalf("digest of a missing tree = %q, %v; want \"\", nil", digest, err)
	}
	if installedJobKey(t, ws, installedExtensionJob(missing, canaryVersion)) ==
		installedJobKey(t, ws, installedExtensionJob(missing, nextVersion)) {
		t.Error("without a tree digest the version no longer keys the task")
	}
}

// The workspace-local identities are unchanged: a local extension with a
// prepared runtime is keyed by its synchronized runtime digest, and one
// without a prepared runtime by its version.
func TestLocalExtensionImplementationDigestIsTheRuntimeDigest(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	job := installedExtensionJob(filepath.Join(ws.Root, "go", "extension"), canaryVersion)
	job.Extension.LocalSource = true
	job.Extension.RelPath = "go/extension"
	job.Extension.Runtime = &extension.RuntimeDefinition{
		Executable: "compiled/putnami-go",
		Prepare:    &extension.RuntimePrepare{Command: "{extensionRoot}/bin/prepare"},
	}
	job.Extension.RuntimeDigest = "synchronized"
	if digest, err := extensionImplementationDigest(ws, job, nil); err != nil || digest != "synchronized" {
		t.Errorf("local runtime digest = %q, %v; want the synchronized digest", digest, err)
	}
	job.Extension.Runtime = nil
	if digest, err := extensionImplementationDigest(ws, job, nil); err != nil || digest != "" {
		t.Errorf("local extension without a runtime = %q, %v; want no digest", digest, err)
	}
}

// Inside the artifact store an installed tree's digest is recorded per archive
// digest and read back by later runs; a record of another schema or a corrupt
// one is derived again. Outside the store a file of the same name is payload,
// never a record.
func TestInstalledExtensionDigestRecordIsTrustedOnlyInsideTheStore(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-store-record-is-trusted-only-inside-the-store")
	storeRoot := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)
	ws := makeExecutorTestWorkspace(t)
	artifacts := artifactstore.New(storeRoot)
	archive := strings.Repeat("ab", 32)
	entry, err := artifacts.Admit(archive, func(stage string) error {
		writeInstalledExtension(t, stage, canaryVersion, "  ")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws.Root, ".putnami", "bin", "extensions", "putnami-go")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entry, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	job := installedExtensionJob(link, canaryVersion)
	digestNow := func() string {
		t.Helper()
		cache := store.NewCacheManager(nil)
		digest, err := extensionImplementationDigest(ws, job, cache)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	recordPath := filepath.Join(entry, "implementationdigest")

	derived, err := installedTreeDigest(entry, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := digestNow(); got != derived {
		t.Fatalf("store entry digest = %s, want the tree digest %s", got, derived)
	}
	recorded, err := os.ReadFile(recordPath)
	if err != nil || string(recorded) != installedTreeDigestSchema+" "+derived {
		t.Fatalf("record = %q, %v; want the schema-tagged tree digest", recorded, err)
	}
	// The record, not the payload, answers a later run.
	planted := strings.Repeat("0", 64)
	writeTestFile(t, recordPath, installedTreeDigestSchema+" "+planted)
	if got := digestNow(); got != planted {
		t.Fatalf("a later run digested %s instead of reading the record %s", got, planted)
	}
	// The record never reaches the digest of the payload it records.
	if again, err := installedTreeDigest(entry, true); err != nil || again != derived {
		t.Fatalf("the record moved the payload digest: %s, %v", again, err)
	}
	for name, record := range map[string]string{
		"another schema": "putnami-installed-extension-tree-v0 " + planted,
		"corrupt":        "garbage",
		"truncated":      installedTreeDigestSchema + " " + planted[:10],
	} {
		writeTestFile(t, recordPath, record)
		if got := digestNow(); got != derived {
			t.Errorf("%s record: digest = %s, want the derived %s", name, got, derived)
		}
		if replaced, _ := os.ReadFile(recordPath); string(replaced) != installedTreeDigestSchema+" "+derived {
			t.Errorf("%s record was not replaced: %q", name, replaced)
		}
	}

	// The same tree outside the store: a file named like the record is part of
	// the payload, so it moves the digest and is never read as an answer.
	outside := filepath.Join(t.TempDir(), "putnami-go")
	writeInstalledExtension(t, outside, canaryVersion, "  ")
	writeTestFile(t, filepath.Join(outside, "implementationdigest"), installedTreeDigestSchema+" "+planted)
	job = installedExtensionJob(outside, canaryVersion)
	got := digestNow()
	if got == planted {
		t.Fatal("a record outside the store was trusted")
	}
	if got != mustTreeDigest(t, outside) || got == derived {
		t.Errorf("outside the store the file is payload: digest %s, store digest %s", got, derived)
	}
}

// A tree laid out as an entry of another artifact store, such as one an
// install under another artifact directory linked, has the identity of its
// payload: the bookkeeping any store rewrites at an entry's root stays out of
// the digest, and a record there is neither read nor written, because only
// this store's entries are trusted.
func TestAnotherStoresEntryIsDigestedWithoutItsBookkeeping(t *testing.T) {
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	ws := makeExecutorTestWorkspace(t)
	other := artifactstore.New(t.TempDir())
	entry, err := other.Admit(strings.Repeat("cd", 32), func(stage string) error {
		writeInstalledExtension(t, stage, canaryVersion, "  ")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	job := installedExtensionJob(entry, canaryVersion)
	digestNow := func() string {
		t.Helper()
		digest, err := extensionImplementationDigest(ws, job, store.NewCacheManager(nil))
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	payload := filepath.Join(t.TempDir(), "payload")
	writeInstalledExtension(t, payload, canaryVersion, "  ")
	want := mustTreeDigest(t, payload)

	if got := digestNow(); got != want {
		t.Fatalf("another store's entry digests %s, want its payload's %s", got, want)
	}
	planted := installedTreeDigestSchema + " " + strings.Repeat("0", 64)
	for name, content := range map[string]string{
		"lastused":                    "1759572000000000000",
		"lastused-123":                "1759572000000000001",
		".lastused.42":                "1759572000000000002",
		"implementationdigest":        planted,
		"implementationdigest-123456": planted,
	} {
		writeTestFile(t, filepath.Join(entry, name), content)
	}
	if got := digestNow(); got != want {
		t.Errorf("that store's bookkeeping moved the digest to %s, want %s", got, want)
	}
	if record, err := os.ReadFile(filepath.Join(entry, "implementationdigest")); err != nil || string(record) != planted {
		t.Errorf("the record of another store was rewritten: %q, %v", record, err)
	}
	// Below the entry's root, a file of a bookkeeping name is payload.
	writeTestFile(t, filepath.Join(entry, "config", "lastused"), "payload")
	if got := digestNow(); got == want {
		t.Error("a payload file named like bookkeeping below the root left the digest unchanged")
	}
}

// A runtime toolchain pin keys the task through the toolchain field, whatever
// the implementation digest: the same installed tree under a new pin misses.
func TestInstalledExtensionKeyMovesWithAToolchainPin(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-toolchain-pin-still-moves-the-key")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	ws := makeExecutorTestWorkspace(t)
	root := filepath.Join(t.TempDir(), "installed")
	writeInstalledExtension(t, root, canaryVersion, "  ")
	keyWith := func(identity string) string {
		t.Helper()
		job := installedExtensionJob(root, canaryVersion)
		job.JobDef.Toolchains = []string{"go"}
		job.Extension.RuntimeToolchains = map[string]model.RuntimeToolchainResolution{
			"go": {Available: true, Identity: identity},
		}
		return installedJobKey(t, ws, job)
	}
	if keyWith("go1.25.7") == keyWith("go1.25.8") {
		t.Fatal("a toolchain pin did not move the key of an installed extension's task")
	}
}
