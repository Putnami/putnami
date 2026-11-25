package artifactstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/store"
)

func TestGCRemovesIdleRecordsAndKeepsRecentOnes(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	now := time.Now()
	idle := filepath.Join(root, store.InputResolveDir, "ab", "idle")
	recent := filepath.Join(root, store.ToolchainProbeDir, "recent")
	old := filepath.Join(root, store.ToolchainProbeDir, "old")
	for _, path := range []string{idle, recent, old} {
		writeFile(t, path, "{}", 0o600)
	}
	for _, path := range []string{idle, old} {
		stamp := now.Add(-40 * 24 * time.Hour)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.GC(GCOptions{Now: now, MaxIdle: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.SweptRecords != 2 {
		t.Fatalf("SweptRecords = %d, want 2", res.SweptRecords)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("a recent record was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(idle)); !os.IsNotExist(err) {
		t.Fatalf("an emptied shard survived: %v", err)
	}
}
