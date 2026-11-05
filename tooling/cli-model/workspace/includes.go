package workspace

import (
	"path/filepath"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ScopePaths returns configured autonomous scope entries.
func ScopePaths(root string, cfg *wsproto.Config) []string {
	if cfg == nil {
		return nil
	}

	seen := make(map[string]bool)
	paths := make([]string, 0, len(cfg.Includes))

	for _, entry := range cfg.Includes {
		path := wsproto.CleanWorkspacePath(entry)
		if path == "" || seen[path] || !isAutonomousScope(root, path) {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}

	return paths
}

// RootProjectPaths returns direct project entries declared at workspace level.
// It excludes include entries that resolve to autonomous scopes.
func RootProjectPaths(root string, cfg *wsproto.Config) []string {
	if cfg == nil {
		return nil
	}

	seen := make(map[string]bool)
	paths := make([]string, 0, len(cfg.Includes))

	for _, entry := range cfg.Includes {
		path := wsproto.CleanWorkspacePath(entry)
		if path == "" || seen[path] || isAutonomousScope(root, path) {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}

	return paths
}

func isAutonomousScope(root, path string) bool {
	sc := wsproto.ReadScopeConfig(filepath.Join(root, filepath.FromSlash(path)))
	return sc != nil && len(sc.IncludePaths()) > 0
}
