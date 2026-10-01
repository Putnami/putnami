package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveBiome_NotFound(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	workspaceDir := filepath.Join(root, "workspace")
	os.MkdirAll(projectDir, 0o755)
	os.MkdirAll(workspaceDir, 0o755)

	// Override PATH so biome cannot be found via exec.LookPath either.
	t.Setenv("PATH", root)

	_, err := ResolveBiome(projectDir, workspaceDir)
	if err == nil {
		t.Fatal("expected error when biome is not found, got nil")
	}
}

func TestResolveBiome_InNodeModules(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	binDir := filepath.Join(projectDir, "node_modules", ".bin")
	os.MkdirAll(binDir, 0o755)

	// The shim a package manager writes on this host: biome on Unix, biome.exe
	// on Windows, where the extensionless shell script is never started.
	biomePath := filepath.Join(binDir, nodeBinShims("biome", runtime.GOOS)[0])
	os.WriteFile(biomePath, []byte("#!/bin/sh"), 0o755)

	workspaceDir := filepath.Join(root, "workspace")
	os.MkdirAll(workspaceDir, 0o755)

	got, err := ResolveBiome(projectDir, workspaceDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != biomePath {
		t.Errorf("expected %q, got %q", biomePath, got)
	}
}

func TestResolveBiomeConfig_InProject(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	workspaceDir := filepath.Join(root, "workspace")
	extensionDir := filepath.Join(root, "extension")
	os.MkdirAll(projectDir, 0o755)
	os.MkdirAll(workspaceDir, 0o755)
	os.MkdirAll(extensionDir, 0o755)

	os.WriteFile(filepath.Join(projectDir, "biome.json"), []byte("{}"), 0o644)

	got := ResolveBiomeConfig(projectDir, workspaceDir, extensionDir)
	expected := filepath.Join(projectDir, "biome.json")
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestResolveBiomeConfig_InWorkspace(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	workspaceDir := filepath.Join(root, "workspace")
	extensionDir := filepath.Join(root, "extension")
	os.MkdirAll(projectDir, 0o755)
	os.MkdirAll(workspaceDir, 0o755)
	os.MkdirAll(extensionDir, 0o755)

	// No biome.json in project, but present in workspace.
	os.WriteFile(filepath.Join(workspaceDir, "biome.json"), []byte("{}"), 0o644)

	got := ResolveBiomeConfig(projectDir, workspaceDir, extensionDir)
	expected := filepath.Join(workspaceDir, "biome.json")
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestResolveBiomeConfig_Fallback(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	workspaceDir := filepath.Join(root, "workspace")
	extensionDir := filepath.Join(root, "extension")
	os.MkdirAll(projectDir, 0o755)
	os.MkdirAll(workspaceDir, 0o755)
	os.MkdirAll(extensionDir, 0o755)

	// No biome.json anywhere → falls back to extensionRoot/config.
	got := ResolveBiomeConfig(projectDir, workspaceDir, extensionDir)
	expected := filepath.Join(extensionDir, "config", "biome.json")
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestResolveBiomeConfig_RelativeExtensionRootIsWorkspaceRelative(t *testing.T) {
	workspaceDir := t.TempDir()
	projectDir := filepath.Join(workspaceDir, "sites", "putnami.dev")
	extensionDir := filepath.Join(workspaceDir, "typescript", "extension")
	os.MkdirAll(projectDir, 0o755)
	os.MkdirAll(filepath.Join(extensionDir, "config"), 0o755)

	got := ResolveBiomeConfig(projectDir, workspaceDir, "typescript/extension")
	expected := filepath.Join(extensionDir, "config", "biome.json")
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}
