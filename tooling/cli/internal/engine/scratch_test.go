package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestExecuteReclaimsExpiredScratchGeneration(t *testing.T) {
	if !flock.Inheritable {
		t.Skip("scratch reaping requires a lease that passes to child processes")
	}
	f := newExecuteFixture(t)
	scratch := filepath.Join(f.wsRoot, ".putnami", "cache")
	orphan := filepath.Join(scratch, "arbitrary-extension", "deleted-project", "binary")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(f.wsRoot, ".putnami", "cache-generation")
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project},
		&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil)
	if result.ExitCode != ExitSuccess {
		t.Fatalf("execution failed: %d", result.ExitCode)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan survived an expired scratch generation: %v", err)
	}
}
