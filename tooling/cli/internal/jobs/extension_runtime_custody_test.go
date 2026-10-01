package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// Preparing the runtime of an extension that is not installed from the
// artifact store starts its code, its prepare command or its runtime-info
// handshake, which is repository code: a hosted run records it before the
// first spawn, and hands its credential to no process started after. The
// runtime of a store-installed extension is registry code.
func TestPreparingARuntimeOutsideTheStoreRecordsRepositoryCode(t *testing.T) {
	artifacts := artifactstore.New(t.TempDir())
	stored := filepath.Join(artifacts.Root(), "extensions", "acme-tool@1.0.0")
	if err := os.MkdirAll(stored, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		artifacts *artifactstore.Store
		path      string
		local     bool
		records   bool
	}{
		"installed from the store": {artifacts: artifacts, path: stored},
		"a local source":           {artifacts: artifacts, path: t.TempDir(), local: true, records: true},
		"outside the store":        {artifacts: artifacts, path: t.TempDir(), records: true},
		"without a store":          {path: stored, records: true},
	} {
		restore := runcredential.SetForTest("run-bearer")
		ext := &extension.ExtensionDescription{
			Name: "@acme/tool", Version: "1.0.0", Path: c.path, LocalSource: c.local,
			Runtime: &extension.RuntimeDefinition{Executable: "missing-runtime"},
		}
		// The runtime is missing, so the preparation fails before any spawn;
		// the record comes first.
		if _, err := prepareOrLoadExtensionRuntime(context.Background(), c.artifacts, ext); err == nil {
			t.Errorf("%s: preparing a missing runtime succeeded", name)
		}
		err := runcredential.RequireCustody("the cache provider of @acme/cache")
		restore()
		if recorded := err != nil; recorded != c.records {
			t.Errorf("%s: repository code recorded = %v (%v), want %v", name, recorded, err, c.records)
		}
	}
}
