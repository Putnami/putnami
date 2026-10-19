package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// The acceptance cases of `putnami extensions install <name>`: naming an
// extension is the request for its agent content. They run the real command
// entry point against a real workspace, with the same packaged extensions and
// registry double as the agent-content lifecycle cases.

const (
	plainExtensionName      = "@fixture/plain"
	dependencyExtensionName = "@fixture/dependency"
)

// runExtensionsInstall runs `putnami extensions install args...` in root.
func runExtensionsInstall(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	var human strings.Builder
	stdout, err := captureStdout(t, func() error {
		return ExtensionsInstallNamed(context.Background(), root, wsproto.Load(root), args, "", LifecycleEnv{Out: &human})
	})
	return stdout + human.String(), err
}

// runExtensionsInstallJSONL runs the command under --output=jsonl and returns
// what reached stdout and the lifecycle actions the command recorded.
func runExtensionsInstallJSONL(t *testing.T, root string, args ...string) (string, []LifecycleAction, error) {
	t.Helper()
	var actions []LifecycleAction
	env := LifecycleEnv{Out: io.Discard, OnAction: func(action LifecycleAction) { actions = append(actions, action) }}
	stdout, err := captureStdout(t, func() error {
		return ExtensionsInstallNamed(context.Background(), root, wsproto.Load(root), args, "jsonl", env)
	})
	return stdout, actions, err
}

// workspaceDeclarations is the part of putnami.workspace.json this feature writes.
type workspaceDeclarations struct {
	Name           string          `json:"name"`
	Extensions     json.RawMessage `json:"extensions"`
	AgentArtifacts []string        `json:"agentArtifacts"`
}

func readDeclarations(t *testing.T, root string) workspaceDeclarations {
	t.Helper()
	var declared workspaceDeclarations
	if err := json.Unmarshal([]byte(readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename)), &declared); err != nil {
		t.Fatalf("parse the workspace file: %v", err)
	}
	return declared
}

func declaredExtensionNames(t *testing.T, root string) []string {
	t.Helper()
	return wsproto.Load(root).Extensions.Names()
}

// packagePlainExtension is a published extension that ships no agent content.
func packagePlainExtension(t *testing.T) *sdkagentartifact.ExtensionPackage {
	t.Helper()
	manifest := `{"name":"` + plainExtensionName + `","version":"1.0.0","commands":{}}`
	archive := buildManifestArchive(t, extension.ManifestFilename, manifest)
	return &sdkagentartifact.ExtensionPackage{
		Name: plainExtensionName, Version: "1.0.0", Manifest: []byte(manifest),
		Archive: archive, ArchiveSHA256: sha256Hex(archive),
	}
}

// packageDependentExtension is a content extension that requires another one.
func packageDependentExtension(t *testing.T, name, dependency, version string) *sdkagentartifact.ExtensionPackage {
	t.Helper()
	files := contentExtensionSourceFiles(name, "r1", false)
	files["putnami.extension.json"] = `{"name":"` + name + `","extensionDependencies":{"` + dependency + `":""},"agentContent":{"path":"agent-content","source":"agent-src"}}`
	source := t.TempDir()
	writeTree(t, source, files)
	pkg, err := sdkagentartifact.PackageExtension(source, version)
	if err != nil {
		t.Fatalf("package %s@%s: %v", name, version, err)
	}
	return pkg
}

// Naming an extension that ships agent content installs it, declares it,
// opts the workspace into its content and materializes the content, in one
// command. A second run and a later `putnami install` change nothing.
func TestExtensionsInstallNamed_OptsIntoTheContentItShips(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "extensions-install-opts-into-the-content-it-ships")

	t.Run("a fresh install, a second run and an install", func(t *testing.T) {
		registry := newContentRegistry(t)
		registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
		root := t.TempDir()
		writeAgentFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"ws","includes":["apps/*"]}`+"\n")

		out, err := runExtensionsInstall(t, root, contentExtensionName)
		if err != nil {
			t.Fatalf("extensions install: %v\n%s", err, out)
		}
		declared := readDeclarations(t, root)
		if declared.Name != "ws" || !reflect.DeepEqual(declaredExtensionNames(t, root), []string{contentExtensionName}) ||
			!reflect.DeepEqual(declared.AgentArtifacts, []string{contentExtensionOptIn}) {
			t.Fatalf("workspace file = %s", readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename))
		}
		if !strings.Contains(readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename), `"includes"`) {
			t.Errorf("the rewrite dropped another member:\n%s", readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename))
		}
		if pin, ok := extensionPin(t, root, contentExtensionName); !ok || pin.Version != "1.0.0" {
			t.Fatalf("extension pin = %+v (present %v)", pin, ok)
		}
		if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision r1.") {
			t.Fatalf("Claude host entry = %q", got)
		}
		if state, err := agentartifacts.LoadState(root, contentExtensionName); err != nil || state == nil {
			t.Fatalf("ownership state = %+v, err %v", state, err)
		}
		assertNoAgentArtifactPins(t, root)

		before := snapshotWorkspace(t, root, installStateMarker)
		if out, err := runExtensionsInstall(t, root, contentExtensionName); err != nil {
			t.Fatalf("second extensions install: %v\n%s", err, out)
		}
		assertUnchangedWorkspace(t, before, snapshotWorkspace(t, root, installStateMarker))

		// `putnami install` also writes the assistant context, so the repeat is
		// held to the declarations and the content, not to the whole tree.
		configBefore := readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename)
		contentBefore := snapshotContentSurface(t, root)
		if out, err := runInstall(t, root); err != nil {
			t.Fatalf("install: %v\n%s", err, out)
		}
		if got := readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename); got != configBefore {
			t.Errorf("install rewrote the workspace file:\n%s", got)
		}
		assertUnchangedWorkspace(t, contentBefore, snapshotContentSurface(t, root))
	})

	t.Run("an extensions object keeps its form", func(t *testing.T) {
		registry := newContentRegistry(t)
		registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
		root := t.TempDir()
		writeAgentFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"ws","extensions":{}}`+"\n")

		if out, err := runExtensionsInstall(t, root, contentExtensionName); err != nil {
			t.Fatalf("extensions install: %v\n%s", err, out)
		}
		var declared struct {
			Extensions map[string]string `json:"extensions"`
		}
		if err := json.Unmarshal([]byte(readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename)), &declared); err != nil {
			t.Fatal(err)
		}
		if _, ok := declared.Extensions[contentExtensionName]; !ok || len(declared.Extensions) != 1 {
			t.Fatalf("extensions = %v, want the object to declare %s", declared.Extensions, contentExtensionName)
		}
		readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md")
	})

	t.Run("an extension the workspace already declares is only opted in", func(t *testing.T) {
		registry := newContentRegistry(t)
		registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
		root := contentWorkspace(t, []string{contentExtensionName}, nil)

		if out, err := runExtensionsInstall(t, root, contentExtensionName); err != nil {
			t.Fatalf("extensions install: %v\n%s", err, out)
		}
		declared := readDeclarations(t, root)
		if !reflect.DeepEqual(declaredExtensionNames(t, root), []string{contentExtensionName}) ||
			!reflect.DeepEqual(declared.AgentArtifacts, []string{contentExtensionOptIn}) {
			t.Fatalf("workspace file = %s", readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename))
		}
	})
}

// An extension without agent content installs as before and writes nothing to
// the workspace file.
func TestExtensionsInstallNamed_ExtensionWithoutContentWritesNoOptIn(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "extensions-install-writes-no-opt-in-without-content")
	registry := newContentRegistry(t)
	registry.publish(packagePlainExtension(t))
	root := t.TempDir()
	writeAgentFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"ws"}`+"\n")
	configBefore := readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename)

	if out, err := runExtensionsInstall(t, root, plainExtensionName); err != nil {
		t.Fatalf("extensions install: %v\n%s", err, out)
	}
	if _, ok := extensionPin(t, root, plainExtensionName); !ok {
		t.Fatal("the extension itself must still install")
	}
	if got := readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename); got != configBefore {
		t.Errorf("the workspace file changed:\n%s", got)
	}
	assertNoAgentSurface(t, root)
}

// An extension installed only because the named one requires it is not opted
// in, even when it ships agent content.
func TestExtensionsInstallNamed_DependencyIsNotOptedIn(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "extensions-install-opts-in-only-the-extension-it-names")
	registry := newContentRegistry(t)
	registry.publish(packageContentExtension(t, dependencyExtensionName, "dep", "1.0.0", false))
	registry.publish(packageDependentExtension(t, contentExtensionName, dependencyExtensionName, "1.0.0"))
	root := t.TempDir()
	writeAgentFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"ws"}`+"\n")

	if out, err := runExtensionsInstall(t, root, contentExtensionName); err != nil {
		t.Fatalf("extensions install: %v\n%s", err, out)
	}
	if _, ok := extensionPin(t, root, dependencyExtensionName); !ok {
		t.Fatal("the dependency must be installed with the extension that requires it")
	}
	declared := readDeclarations(t, root)
	if !reflect.DeepEqual(declaredExtensionNames(t, root), []string{contentExtensionName}) ||
		!reflect.DeepEqual(declared.AgentArtifacts, []string{contentExtensionOptIn}) {
		t.Fatalf("workspace file = %s", readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename))
	}
	if state, err := agentartifacts.LoadState(root, dependencyExtensionName); err != nil || state != nil {
		t.Fatalf("the dependency's content was installed: %+v, err %v", state, err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".claude", "skills", "fixture-review", "SKILL.md")); err != nil {
		t.Errorf("the named extension's content is missing: %v", err)
	}
}

const secondContentExtensionName = "@fixture/second"

// packageSecondContentExtension is a content extension whose skills and agents
// carry other names than the fixture contributor's, so both can be
// materialized into one workspace without sharing a path.
func packageSecondContentExtension(t *testing.T) *sdkagentartifact.ExtensionPackage {
	t.Helper()
	files := map[string]string{}
	for path, content := range contentExtensionSourceFiles(secondContentExtensionName, "s1", false) {
		files[strings.ReplaceAll(path, "fixture-review", "second-review")] = strings.ReplaceAll(content, "fixture-review", "second-review")
	}
	source := t.TempDir()
	writeTree(t, source, files)
	pkg, err := sdkagentartifact.PackageExtension(source, "1.0.0")
	if err != nil {
		t.Fatalf("package %s: %v", secondContentExtensionName, err)
	}
	return pkg
}

// Naming an extension materializes the content of that extension only. A
// workspace that already opts into a second extension, whose content cannot be
// materialized here, keeps that extension's files and ownership record as they
// are, and the command does not fail on it.
func TestExtensionsInstallNamed_MaterializesOnlyTheNamedContent(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "extensions-install-materializes-only-the-content-it-names")
	registry := newContentRegistry(t)
	registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
	registry.publish(packageSecondContentExtension(t))
	root := t.TempDir()
	writeAgentFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"ws"}`+"\n")

	if out, err := runExtensionsInstall(t, root, secondContentExtensionName); err != nil {
		t.Fatalf("install the second extension: %v\n%s", err, out)
	}
	secondFiles := []string{
		".claude/skills/second-review/SKILL.md",
		".agents/skills/second-review/SKILL.md",
		".codex/agents/second-reviewer.toml",
	}
	for _, path := range secondFiles {
		readAgentFile(t, root, path)
	}
	// The second extension's content can no longer be read: its pin is gone.
	lock, err := lockfile.ReadLockFile(root)
	if err != nil || lock == nil {
		t.Fatalf("read lock: %v (%v)", lock, err)
	}
	lock.RemoveExtension(secondContentExtensionName)
	if err := lockfile.WriteLockFile(root, lock); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := agentartifacts.LoadState(root, secondContentExtensionName)
	if err != nil || stateBefore == nil {
		t.Fatalf("ownership state of the second extension = %+v, err %v", stateBefore, err)
	}
	secondBefore := map[string]string{}
	for path, digest := range snapshotContentSurface(t, root) {
		secondBefore[path] = digest
	}

	if out, err := runExtensionsInstall(t, root, contentExtensionName); err != nil {
		t.Fatalf("extensions install %s: %v\n%s", contentExtensionName, err, out)
	}
	if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision r1.") {
		t.Fatalf("the named extension's content = %q", got)
	}
	after := snapshotContentSurface(t, root)
	for path, digest := range secondBefore {
		if after[path] != digest {
			t.Errorf("%s changed: %s -> %s", path, digest, after[path])
		}
	}
	for _, path := range secondFiles {
		if _, ok := after[path]; !ok {
			t.Errorf("%s was deleted", path)
		}
	}
	if state, err := agentartifacts.LoadState(root, secondContentExtensionName); err != nil || !reflect.DeepEqual(state, stateBefore) {
		t.Errorf("ownership state of the second extension = %+v (err %v), want it untouched", state, err)
	}
	if state, err := agentartifacts.LoadState(root, contentExtensionName); err != nil || state == nil {
		t.Errorf("ownership state of the named extension = %+v, err %v", state, err)
	}
	if got := readDeclarations(t, root).AgentArtifacts; !reflect.DeepEqual(got, []string{"extension:" + secondContentExtensionName, contentExtensionOptIn}) {
		t.Errorf("agentArtifacts = %v", got)
	}
}

// When the content step fails after the opt-in was written, the workspace
// file keeps both declarations and the lock keeps the pin. The error names
// `putnami install`, which finishes the job once the cause is gone.
func TestExtensionsInstallNamed_ContentFailureKeepsTheOptIn(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "extensions-install-keeps-the-opt-in-when-the-content-fails")
	registry := newContentRegistry(t)
	registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
	root := t.TempDir()
	writeAgentFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"ws"}`+"\n")
	const owned = ".claude/skills/fixture-review/SKILL.md"
	writeAgentFile(t, root, owned, "# my own skill\n")

	_, err := runExtensionsInstall(t, root, contentExtensionName)
	if !errors.Is(err, agentartifacts.ErrCollision) || !strings.Contains(err.Error(), owned+" [unmanaged]") {
		t.Fatalf("error = %v, want a collision naming %s", err, owned)
	}
	if next := protocolcli.SuggestedNext(err); next != "putnami install" {
		t.Errorf("next step = %q, want putnami install", next)
	}
	declared := readDeclarations(t, root)
	if !reflect.DeepEqual(declaredExtensionNames(t, root), []string{contentExtensionName}) ||
		!reflect.DeepEqual(declared.AgentArtifacts, []string{contentExtensionOptIn}) {
		t.Fatalf("workspace file = %s", readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename))
	}
	if pin, ok := extensionPin(t, root, contentExtensionName); !ok || pin.Version != "1.0.0" {
		t.Fatalf("extension pin = %+v (present %v)", pin, ok)
	}
	if got := readAgentFile(t, root, owned); got != "# my own skill\n" {
		t.Fatalf("the user's file was overwritten: %q", got)
	}
	assertFileMissing(t, root, ".agents/skills/fixture-review/SKILL.md")
	assertFileMissing(t, root, ".putnami/agent-artifacts")

	// The next step works once the file is out of the way.
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(owned))); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstall(t, root); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := readAgentFile(t, root, owned); !strings.Contains(got, "Review revision r1.") {
		t.Fatalf("content after install = %q", got)
	}
}

// Under --output=jsonl stdout carries only the install records, and the
// opt-in and the materialization are reported as lifecycle actions, the way
// `putnami install` reports its agent phase.
func TestExtensionsInstallNamed_ReportsActionsUnderJSONL(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "extensions-install-reports-the-opt-in-and-the-content")
	registry := newContentRegistry(t)
	registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
	root := t.TempDir()
	writeAgentFile(t, root, wsproto.WorkspaceConfigFilename, `{"name":"ws"}`+"\n")

	stdout, actions, err := runExtensionsInstallJSONL(t, root, contentExtensionName)
	if err != nil {
		t.Fatalf("extensions install: %v\n%s", err, stdout)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var record struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(line), &record) != nil || record.Kind != "extension" {
			t.Errorf("stdout line %q is not an extension install record", line)
		}
	}
	want := []LifecycleAction{
		{
			Kind: "workspace-config", Name: wsproto.WorkspaceConfigFilename,
			Description: "Opted into the agent content of " + contentExtensionName + " in " + wsproto.WorkspaceConfigFilename,
		},
		{
			Kind: "agent-workflow", Name: contentExtensionName,
			Description: "Agent workflow " + contentExtensionName + "@1.0.0 materialized",
		},
	}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions = %+v, want %+v", actions, want)
	}

	// A second run changes nothing, so it records nothing.
	if _, actions, err := runExtensionsInstallJSONL(t, root, contentExtensionName); err != nil || len(actions) != 0 {
		t.Fatalf("second run: actions %+v, err %v", actions, err)
	}
}

// An agentArtifacts member that is not an array is refused with the existing
// message, before the workspace file is written.
func TestExtensionsInstallNamed_AgentArtifactsObjectIsRefused(t *testing.T) {
	registry := newContentRegistry(t)
	registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
	root := t.TempDir()
	config := `{"name":"ws","agentArtifacts":{}}` + "\n"
	writeAgentFile(t, root, wsproto.WorkspaceConfigFilename, config)

	_, err := runExtensionsInstall(t, root, contentExtensionName)
	if err == nil || !strings.Contains(err.Error(), "agentArtifacts must be an array") {
		t.Fatalf("error = %v, want agentArtifacts must be an array", err)
	}
	if got := readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename); got != config {
		t.Errorf("the workspace file changed:\n%s", got)
	}
	assertNoAgentSurface(t, root)
}
