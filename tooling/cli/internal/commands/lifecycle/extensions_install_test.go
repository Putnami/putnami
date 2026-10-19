package lifecycle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/extension"
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
	return captureStdout(t, func() error {
		return ExtensionsInstallNamed(context.Background(), root, wsproto.Load(root), args, "")
	})
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
