package cli

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The credential seam mints a bearer for one host. Every Go job of a run starts
// it, so it must not resolve the version lines, which walks the work tree once
// per job. Any other interactive command still gets its lines.
func TestInteractiveCommandVersionsSkipsTheCredentialSeam(t *testing.T) {
	t.Parallel()
	ws := workspace.NewWorkspace(t.TempDir(), nil, nil)
	resolved := 0
	resolve := func(*workspace.Workspace) (cachekey.RunVersions, error) {
		resolved++
		return cachekey.RunVersions{"app": {Base: "1.2.3", Line: "app"}}, nil
	}

	versions, err := interactiveCommandVersions(ws, "cloud", "registry-token", resolve)
	if err != nil || versions != nil || resolved != 0 {
		t.Fatalf("seam versions = %v, %v after %d resolutions, want none and no resolution", versions, err, resolved)
	}

	versions, err = interactiveCommandVersions(ws, "cloud", "release-set", resolve)
	if err != nil || resolved != 1 || versions["app"] == nil || versions["app"].Base != "1.2.3" {
		t.Fatalf("release-set versions = %+v, %v after %d resolutions, want the resolved lines", versions, err, resolved)
	}
}
