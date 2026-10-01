package toolchain

import (
	"fmt"
	"path/filepath"
)

// ResolveBiome finds the file that starts biome for a project. In the nearest
// node_modules directory, from the project up to the workspace, that is the
// native executable of the platform package installed for the host
// (@biomejs/cli-<os>-<arch>, with a -musl suffix on a musl host), else the
// launcher in node_modules/.bin; then a biome on PATH. The launcher is a
// JavaScript file: Command starts what this returns.
func ResolveBiome(projectRoot, workspaceRoot string) (string, error) {
	return resolveBiomeOn(hostPlatform(), projectRoot, workspaceRoot)
}

// resolveBiomeOn is ResolveBiome for a process that runs on p.
func resolveBiomeOn(p platform, projectRoot, workspaceRoot string) (string, error) {
	path := resolveBinary("biome", projectRoot, workspaceRoot, p)
	if path == "" {
		return "", fmt.Errorf("biome not found: install @biomejs/biome as a devDependency")
	}
	return path, nil
}

// ResolveBiomeConfig resolves the biome configuration file.
// Resolution: project biome.json → workspace biome.json → extension default.
func ResolveBiomeConfig(projectRoot, workspaceRoot, extensionRoot string) string {
	projectRoot = absolutePath(projectRoot, workspaceRoot)
	workspaceRoot = absolutePath(workspaceRoot, "")
	extensionRoot = absolutePath(extensionRoot, workspaceRoot)

	// Check project-level biome.json
	projectConfig := filepath.Join(projectRoot, "biome.json")
	if fileExists(projectConfig) {
		return projectConfig
	}
	// Check workspace-level biome.json
	workspaceConfig := filepath.Join(workspaceRoot, "biome.json")
	if fileExists(workspaceConfig) {
		return workspaceConfig
	}
	// Fall back to extension default config
	return filepath.Join(extensionRoot, "config", "biome.json")
}

func absolutePath(path, workspaceRoot string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if workspaceRoot != "" && filepath.IsAbs(workspaceRoot) {
		return filepath.Join(workspaceRoot, path)
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
