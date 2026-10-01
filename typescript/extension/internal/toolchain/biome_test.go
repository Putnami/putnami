package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
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

// With every platform package and every shim installed, each host starts the
// native executable of its own platform package.
func TestResolveBiome_NativeProgramOfTheHostPlatformPackage(t *testing.T) {
	root := t.TempDir()
	for _, shim := range []string{"biome", "biome.exe", "biome.cmd"} {
		writeShim(t, root, shim)
	}
	want := map[string]string{}
	for _, c := range biomePlatformPackages {
		want[c.pkg] = writeNativeBiome(t, root, c.pkg)
	}
	project := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, c := range biomePlatformPackages {
		t.Run(hostName(c.host), func(t *testing.T) {
			check := "lint-starts-the-native-biome-of-the-platform-package"
			if c.host.musl {
				check = "a-musl-host-starts-the-musl-biome"
			}
			spectest.Proves(t, "typescript/typescript-project-toolchain", "no-node-on-the-host", check)

			got, err := resolveBiomeOn(c.host, project, root)
			if err != nil {
				t.Fatal(err)
			}
			if got != want[c.pkg] {
				t.Errorf("resolveBiomeOn() = %q, want the native executable %q", got, want[c.pkg])
			}
			program, args, err := commandWith(noBun, got, []string{"check"})
			if err != nil {
				t.Fatal(err)
			}
			if program != got || !slices.Equal(args, []string{"check"}) {
				t.Errorf("commandWith() = %q %q, want the native executable started as itself", program, args)
			}
		})
	}
}

// Without the platform package of the host, biome is the JavaScript launcher,
// and bun starts it.
func TestResolveBiome_LauncherStartsThroughBunWithoutThePlatformPackage(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "no-node-on-the-host", "a-missing-platform-package-starts-the-launcher-with-bun")
	root := t.TempDir()
	launcher := writeShim(t, root, "biome")
	if err := os.WriteFile(launcher, []byte("#!/usr/bin/env node\nrequire('child_process');\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeNativeBiome(t, root, "cli-linux-x64-musl")

	for _, host := range []platform{{goos: "linux", goarch: "amd64"}, {goos: "darwin", goarch: "arm64"}} {
		t.Run(hostName(host), func(t *testing.T) {
			got, err := resolveBiomeOn(host, root, root)
			if err != nil {
				t.Fatal(err)
			}
			if got != launcher {
				t.Fatalf("resolveBiomeOn() = %q, want the launcher %q", got, launcher)
			}
			program, args, err := commandWith(func() (string, error) { return "/task/bun", nil }, got, []string{"check", "."})
			if err != nil {
				t.Fatal(err)
			}
			if program != "/task/bun" || !slices.Equal(args, []string{launcher, "check", "."}) {
				t.Errorf("commandWith() = %q %q, want bun started with the launcher and its arguments", program, args)
			}
		})
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
