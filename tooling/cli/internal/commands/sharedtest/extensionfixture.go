package sharedtest

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteContextTestExtension writes a fake extension artifact into a
// content-addressed store fixture at storeRoot (mirroring the real
// PUTNAMI_ARTIFACT_DIR layout: sha256/<prefix>/<digest>/), with a minimal
// putnami.extension.json manifest and an AI.md carrying guidance. It returns
// the artifact directory. Shared by 2+ verticals' tests that need a
// lock-pinned extension resolvable from the store without a real registry
// (agentctx's AI-context generation tests, lifecycle's Install tests).
func WriteContextTestExtension(t *testing.T, storeRoot, digest, name, version, guidance string) string {
	t.Helper()
	dir := filepath.Join(storeRoot, "sha256", digest[:2], digest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir extension store fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(ContextTestExtensionManifest(name, version)), 0o644); err != nil {
		t.Fatalf("write extension manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AI.md"), []byte(guidance), 0o644); err != nil {
		t.Fatalf("write extension guidance: %v", err)
	}
	return dir
}

// ContextTestExtensionManifest is the minimal putnami.extension.json body
// WriteContextTestExtension writes.
func ContextTestExtensionManifest(name, version string) string {
	return `{"name":"` + name + `","version":"` + version + `","commands":{}}`
}
