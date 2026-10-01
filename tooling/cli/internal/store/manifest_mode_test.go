package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Two entries whose outputs hold the same bytes share one CAS blob, and each
// capture chmods that blob to its own mode. A restore applies the mode its own
// manifest recorded, for directory and file outputs alike, whichever entry
// linked the blob last.
func TestMaterializeTaskOutput_AppliesTheManifestModeOfASharedBlob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the host filesystem records no permission bits")
	}
	for _, tc := range []struct {
		name string
		out  DeclaredEntryOutput
		rel  string
		path func(dest string) string
	}{
		{"directory output", dirOutput("dist", "dist"), "bin/tool", func(dest string) string { return filepath.Join(dest, "bin", "tool") }},
		{"file output", fileOutput("tool", "tool"), "", func(dest string) string { return dest }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewLocalStore(t.TempDir())
			ingest := func(key string, mode os.FileMode) *TaskEntry {
				staging := t.TempDir()
				stage(t, staging, tc.out, tc.rel, "#!/bin/sh\nexit 0\n")
				staged := TaskStagingPath(staging, tc.out)
				if tc.rel != "" {
					staged = filepath.Join(staged, filepath.FromSlash(tc.rel))
				}
				if err := os.Chmod(staged, mode); err != nil {
					t.Fatal(err)
				}
				entry, err := s.IngestTaskEntry(staging, taskSpec(key, tc.out))
				if err != nil {
					t.Fatalf("IngestTaskEntry(%s): %v", key, err)
				}
				return entry
			}
			executable := ingest("exec-key", 0o755)
			plain := ingest("plain-key", 0o644)

			for _, restore := range []struct {
				entry *TaskEntry
				want  os.FileMode
			}{{executable, 0o755}, {plain, 0o644}} {
				dest := filepath.Join(t.TempDir(), "out")
				if _, err := s.MaterializeTaskOutput(restore.entry, tc.out.ID, dest, ""); err != nil {
					t.Fatalf("MaterializeTaskOutput: %v", err)
				}
				info, err := os.Stat(tc.path(dest))
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != restore.want {
					t.Errorf("restored mode = %o, want the recorded %o", got, restore.want)
				}
			}
		})
	}
}
