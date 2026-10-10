package deliverycli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestWarmSeedCopiesOnlyManifestBoundLockedArtifacts(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "bound", false: "altered-manifest"}[valid], func(t *testing.T) {
			workspace := imageLayersWarmWorkspaceFixture(t)
			store, scratch, destination := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("PUTNAMI_ARTIFACT_DIR", store)
			var lock map[string]any
			data, err := os.ReadFile(filepath.Join(workspace, "putnami.lock.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &lock); err != nil {
				t.Fatal(err)
			}
			lock["version"] = 3
			extensions := lock["extensions"].(map[string]any)
			manifest := []byte(`{"name":"@putnami/go","version":"0.1.0-8885222db"}`)
			entry := extensions["@putnami/go"].(map[string]any)
			entry["manifestHash"] = sha256Hex(manifest)
			entry["source"] = "https://registry.invalid/host-only-source"
			data, err = json.Marshal(lock)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "putnami.lock.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(store, "sha256", warmGoDigest[:2], warmGoDigest)
			if err := os.MkdirAll(filepath.Join(root, "compiled"), 0o755); err != nil {
				t.Fatal(err)
			}
			if !valid {
				manifest = []byte(`{"name":"unexpected"}`)
			}
			if err := os.WriteFile(filepath.Join(root, "putnami.extension.json"), manifest, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "compiled", "go"), []byte("cached-linux-runtime"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store, "credentials.json"), []byte("must-not-copy"), 0o600); err != nil {
				t.Fatal(err)
			}
			artifacts, err := imageLayersWarmResolve(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err := imageLayersSeedWarmStore(workspace, scratch, destination, artifacts); err != nil {
				t.Fatal(err)
			}
			cached := filepath.Join(destination, "sha256", warmGoDigest[:2], warmGoDigest, "compiled", "go")
			info, err := os.Stat(cached)
			if valid {
				if err != nil || info.Mode().Perm() != 0o755 {
					t.Fatalf("bound executable not preserved: %v", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("altered manifest seeded an artifact: %v", err)
			}
			if _, err := os.Stat(filepath.Join(destination, "credentials.json")); !os.IsNotExist(err) {
				t.Fatal("host credential file entered materialization")
			}
			materializedLock, err := os.ReadFile(filepath.Join(scratch, "putnami.lock.json"))
			if err != nil || strings.Contains(string(materializedLock), "host-only-source") {
				t.Fatalf("scratch lock retained host source metadata: %v", err)
			}
			var selected struct {
				Extensions map[string]imageLayersWarmLockEntry `json:"extensions"`
			}
			if err := json.Unmarshal(materializedLock, &selected); err != nil {
				t.Fatal(err)
			}
			if len(selected.Extensions) != 2 || selected.Extensions["@putnami/go"].ManifestHash != entry["manifestHash"] {
				t.Fatal("installer lost its exact platform/manifest lock")
			}
		})
	}
}

func TestWarmMaterializationUsesTheSpawningCLI(t *testing.T) {
	selected := filepath.Join(t.TempDir(), "selected-putnami")
	t.Setenv("PUTNAMI_CLI_EXECUTABLE", selected)
	previous := imageBuildLookPath
	t.Cleanup(func() { imageBuildLookPath = previous })
	imageBuildLookPath = func(string) (string, error) {
		t.Fatal("materialization selected an unrelated CLI from PATH")
		return "", nil
	}
	got, err := imageLayersWarmCLI()
	if err != nil || got != selected {
		t.Fatalf("selected CLI = %q, %v", got, err)
	}
}

func TestWarmMaterializationAdvertisesCLIToCredentialChild(t *testing.T) {
	for _, advertised := range []bool{false, true} {
		t.Run(map[bool]string{false: "path-fallback", true: "spawning-cli"}[advertised], func(t *testing.T) {
			root := t.TempDir()
			selected := filepath.Join(root, "putnami")
			script := `#!/bin/sh
test "$PUTNAMI_CLI_EXECUTABLE" = "$0" || exit 71
test "$PUTNAMI_NO_AUTO_INSTALL" = 1 || exit 72
if [ "$1" = cloud ]; then
  test "$2" = registry-token || exit 73
  printf fixture-bearer
  exit 0
fi
test "$1:$2" = extensions:install || exit 74
credential=$("$PUTNAMI_CLI_EXECUTABLE" cloud registry-token --host registry.invalid) || exit 75
test "$credential" = fixture-bearer || exit 76
`
			if err := os.WriteFile(selected, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PUTNAMI_CLI_EXECUTABLE", "")
			if advertised {
				t.Setenv("PUTNAMI_CLI_EXECUTABLE", selected)
			}
			previous := imageBuildLookPath
			t.Cleanup(func() { imageBuildLookPath = previous })
			imageBuildLookPath = func(string) (string, error) { return selected, nil }
			if err := imageLayersWarmRun(context.Background(), root, filepath.Join(root, "store"), clicore.IO{}); err != nil {
				t.Fatalf("credential child lost selected CLI: %v", err)
			}
		})
	}
}

func TestWarmWorkspaceCarriesOnlyCloudLinkIdentity(t *testing.T) {
	root := t.TempDir()
	provider := filepath.Join(root, "cloud")
	if err := os.MkdirAll(provider, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(provider, "putnami.extension.json"), []byte(`{"name":"@putnami/cloud"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"extensions": []string{provider},
		"options": map[string]any{"@putnami/cloud": map[string]any{
			"workspace": map[string]any{"workspace_id": "ws-owner", "control_plane_url": "https://api.example.com", "token": "must-not-copy"},
			"unrelated": "must-not-copy",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, imageLayersWarmWorkspaceName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := imageLayersWarmWorkspace(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, imageLayersWarmWorkspaceName), got, 0o600); err != nil {
		t.Fatal(err)
	}
	link, err := clicore.ReadCloudLink(scratch)
	if err != nil || link["workspace_id"] != "ws-owner" || link["control_plane_url"] != "https://api.example.com" {
		t.Fatalf("credential provider lost original workspace: %v", err)
	}
	if strings.Contains(string(got), "must-not-copy") || len(link) != 2 {
		t.Fatal("scratch credential context copied unrelated host data")
	}
}
