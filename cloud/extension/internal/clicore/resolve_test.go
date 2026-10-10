package clicore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedCachedLink writes a cloud-link.json cache under a fresh workspace root
// that has NO manifest link, so ReadCloudLink exercises the cached-fallback
// path (ReadManifestLink short-circuits otherwise). Returns the workspace root.
func seedCachedLink(t *testing.T, data []byte) string {
	t.Helper()
	root := t.TempDir()
	file := LinkPath(root)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatalf("write cloud-link.json: %v", err)
	}
	return root
}

// assertNotConfigured asserts err is the actionable "not configured" ExitUsage
// error rather than a cryptic parse error.
func assertNotConfigured(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("ReadCloudLink = nil error, want not-configured ExitUsage")
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("ReadCloudLink error = %T (%v), want *ExitError", err, err)
	}
	if ee.Code != ExitUsage {
		t.Fatalf("ReadCloudLink exit code = %d, want %d (ExitUsage)", ee.Code, ExitUsage)
	}
	if got, notWant := ee.Message, "parse "; len(got) >= len(notWant) && got[:len(notWant)] == notWant {
		t.Fatalf("ReadCloudLink surfaced a parse error %q; want the not-configured message", got)
	}
}

// TestReadCloudLinkTruncatedCacheIsNotConfigured pins acceptance criterion 3: a
// half-written/truncated cached cloud-link.json (as a killed writer could leave
// on a pre-atomic CLI) must fail fast to the not-configured path, NOT surface a
// cryptic parse error.
func TestReadCloudLinkTruncatedCacheIsNotConfigured(t *testing.T) {
	t.Parallel()
	// A prefix of a valid JSON object — invalid on its own.
	root := seedCachedLink(t, []byte(`{"workspace_id": "w", "control_pl`))

	_, err := ReadCloudLink(root)
	assertNotConfigured(t, err)
}

// TestReadCloudLinkGarbageCacheIsNotConfigured covers non-JSON garbage the same
// way.
func TestReadCloudLinkGarbageCacheIsNotConfigured(t *testing.T) {
	t.Parallel()
	root := seedCachedLink(t, []byte("not json at all\x00\x01"))

	_, err := ReadCloudLink(root)
	assertNotConfigured(t, err)
}

// TestReadCloudLinkEmptyCacheIsNotConfigured covers an empty/whitespace-only
// cache — the zero-length window a truncating write briefly exposes.
func TestReadCloudLinkEmptyCacheIsNotConfigured(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"empty":      "",
		"whitespace": "  \n\t ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := seedCachedLink(t, []byte(body))
			_, err := ReadCloudLink(root)
			assertNotConfigured(t, err)
		})
	}
}

// TestReadCloudLinkValidCacheResolves is the positive control: a complete cache
// with the required fields resolves normally, so the fail-fast change does not
// swallow good links.
func TestReadCloudLinkValidCacheResolves(t *testing.T) {
	t.Parallel()
	root := seedCachedLink(t, []byte(`{"workspace_id":"ws-1","control_plane_url":"https://api.putnami.cloud"}`))

	link, err := ReadCloudLink(root)
	if err != nil {
		t.Fatalf("ReadCloudLink: %v", err)
	}
	if got := StringValue(link["workspace_id"]); got != "ws-1" {
		t.Fatalf("workspace_id = %q, want ws-1", got)
	}
	if got := StringValue(link["control_plane_url"]); got != "https://api.putnami.cloud" {
		t.Fatalf("control_plane_url = %q, want https://api.putnami.cloud", got)
	}
}

func TestReadCloudWorkspaceConfigReturnsLinkAndManifestOptions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manifest := map[string]any{
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{"workspace_id": "ws-1"},
				"intelligence": map[string]any{
					"workspace": "cloud",
					"baseUrl":   "https://intelligence.putnami.cloud",
				},
			},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := ReadCloudWorkspaceConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := StringValue(config.Link["workspace_id"]); got != "ws-1" {
		t.Fatalf("workspace_id = %q, want ws-1", got)
	}
	intelligence, _ := config.Options["intelligence"].(map[string]any)
	if got := StringValue(intelligence["workspace"]); got != "cloud" {
		t.Fatalf("intelligence workspace = %q, want cloud", got)
	}
	if got := StringValue(config.Link["control_plane_url"]); got != DefaultControlPlaneURL {
		t.Fatalf("control_plane_url = %q, want %q", got, DefaultControlPlaneURL)
	}
}
