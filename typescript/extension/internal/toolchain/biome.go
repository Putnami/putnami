package toolchain

import (
	"fmt"
	"path/filepath"
)

// ResolveBiome finds the biome binary.
// Resolution: project node_modules/.bin/ → workspace node_modules/.bin/ → PATH.
func ResolveBiome(projectRoot, workspaceRoot string) (string, error) {
	p := resolveBinary("biome", projectRoot, workspaceRoot)
	if p == "" {
		return "", fmt.Errorf("biome not found: install @biomejs/biome as a devDependency")
	}
	return p, nil
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
