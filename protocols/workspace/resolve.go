// Package workspace provides types and loading logic for putnami workspace,
// project, and scope configuration files.
package workspace

import (
	"os"
	"path/filepath"
)

const (
	// WorkspaceConfigFilename is the workspace-level config file.
	WorkspaceConfigFilename = "putnami.workspace.json"

	// ConfigFilename is the project/scope config file.
	ConfigFilename = "putnami.json"

	// LegacyConfigFilename is the historic project-config spelling. Core reads
	// ConfigFilename for project tuning, but extension task cache declarations
	// still include this name, so path-sensitive protocol consumers share the
	// spelling instead of duplicating it.
	LegacyConfigFilename = ".putnamirc.json"

	// GlobalConfigDir is the directory under $HOME for global config.
	GlobalConfigDir = ".putnami"

	// GlobalConfigFilename is the global config file within GlobalConfigDir.
	GlobalConfigFilename = "config.json"
)

// ResolveFile returns the path to the preferred config file in dir.
func ResolveFile(dir, preferred string) string {
	return filepath.Join(dir, preferred)
}

// IsWorkspaceConfig returns true if dir contains a workspace-level config
// (putnami.workspace.json).
func IsWorkspaceConfig(dir string) bool {
	return fileExists(filepath.Join(dir, WorkspaceConfigFilename))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
