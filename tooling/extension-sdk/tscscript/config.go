package tscscript

import (
	"os"
	"path/filepath"
)

// DefaultConfig is the config tsc -p compiles for a project with no build
// script: tsconfig.lib.json, the library config an Nx project compiles, when
// the project holds one, else tsconfig.json. projectDir is the project directory.
func DefaultConfig(projectDir string) string {
	if info, err := os.Stat(filepath.Join(projectDir, "tsconfig.lib.json")); err == nil && !info.IsDir() {
		return "tsconfig.lib.json"
	}
	return "tsconfig.json"
}
