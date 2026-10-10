package cloudcli

import (
	"path/filepath"
	"testing"
)

// The config/secrets/publish-config command tests live with their sources
// in internal/configcli. What remains here is the shared
// fixture the retained aggregator suites still need: writeLinkFile seeds a
// linked workspace for cache_test.go, cli_test.go, and the RunMain
// reveal-secrets tests in config_test.go.

func writeLinkFile(t *testing.T, workspaceRoot string) {
	t.Helper()
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{
		"name": "test-workspace",
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"version":           1,
					"control_plane_url": testBaseURL,
					"workspace_id":      "ws-acme",
					"environment":       "prod",
				},
			},
		},
	})
	mustMkdir(t, filepath.Join(workspaceRoot, ".putnami"))
	writeJSONFile(t, filepath.Join(workspaceRoot, LinkFileRelative), map[string]any{
		"version":           1,
		"control_plane_url": testBaseURL,
		"workspace_id":      "ws-acme",
		"environment":       "prod",
	})
}
