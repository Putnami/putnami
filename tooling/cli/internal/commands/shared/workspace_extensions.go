package shared

import (
	"fmt"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// DiscoverWorkspaceExtensions discovers the extensions of the workspace at
// wsRoot: the ones cfg declares and the extension manifests the workspace's
// projects hold. The caller chooses cfg, so it decides whether the global
// configuration takes part.
func DiscoverWorkspaceExtensions(wsRoot string, cfg *wsproto.Config) (*extension.DiscoveryResult, error) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return nil, fmt.Errorf("load workspace graph: %w", err)
	}
	projectPaths := make([]string, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		projectPaths = append(projectPaths, project.Path)
	}
	discovered, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		return nil, fmt.Errorf("discover workspace jobs: %w", err)
	}
	return discovered, nil
}
