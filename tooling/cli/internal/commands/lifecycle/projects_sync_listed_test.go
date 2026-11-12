package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// listingProvider answers a probe with the candidates it lists, or fails.
type listingProvider struct {
	name   string
	listed []string
	err    error
}

func (p listingProvider) Name() string { return p.name }

func (p listingProvider) Probe(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	if p.err != nil {
		return wsproto.ProbeResult{}, p.err
	}
	result := wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: p.name}
	for _, path := range request.Paths {
		if slices.Contains(p.listed, path) {
			result.Projects = append(result.Projects, wsproto.ProbeProject{Path: path})
		}
	}
	return result, nil
}

func adaptersOf(providers ...listingProvider) workspaceAdapters {
	var adapters workspaceAdapters
	for _, p := range providers {
		adapters.adapters = append(adapters.adapters, workspaceAdapter{binding: workspace.ProviderBinding{
			Scope:    workspace.ProviderScope{Extension: p.name},
			Provider: p,
		}})
	}
	return adapters
}

// listedWorkspace holds a member with its own putnami.json, which the tool
// does not list.
func listedWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "libs", "own")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsproto.ConfigFilename), []byte(`{"name": "own"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

var scannedMembers = []string{"apps/api", "apps/api/test/fixture", "libs/own"}

func TestAMonorepoToolBoundsTheScannedMembers(t *testing.T) {
	root := listedWorkspace(t)
	kept, unlisted, err := keepListedMembers(context.Background(), root,
		adaptersOf(listingProvider{name: "@putnami/rush", listed: []string{"apps/api"}}), scannedMembers)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"apps/api", "libs/own"}; !slices.Equal(kept, want) {
		t.Errorf("kept %v, want %v: the listed member and the one with its own putnami.json", kept, want)
	}
	if want := []string{"apps/api/test/fixture"}; !slices.Equal(unlisted, want) {
		t.Errorf("unlisted %v, want %v", unlisted, want)
	}
}

func TestWithoutAMonorepoToolTheScanStands(t *testing.T) {
	root := listedWorkspace(t)
	kept, unlisted, err := keepListedMembers(context.Background(), root,
		adaptersOf(listingProvider{name: "@putnami/typescript", listed: []string{"apps/api"}}), scannedMembers)
	if err != nil || !slices.Equal(kept, scannedMembers) || unlisted != nil {
		t.Errorf("kept %v, unlisted %v, err %v; want the scan unchanged", kept, unlisted, err)
	}
}

func TestAToolThatCannotListKeepsOnlyTheMembersWithTheirOwnConfig(t *testing.T) {
	root := listedWorkspace(t)
	kept, unlisted, err := keepListedMembers(context.Background(), root,
		adaptersOf(listingProvider{name: "@putnami/nx", err: errors.New("nx graph failed")}), scannedMembers)
	if err == nil {
		t.Fatal("a failed listing reports no error")
	}
	if want := []string{"libs/own"}; !slices.Equal(kept, want) || unlisted != nil {
		t.Errorf("kept %v, unlisted %v; want %v and none unlisted", kept, unlisted, want)
	}
}
