//go:build unix

package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

// Resolving a descendant requires traversing its parent first. Hold that
// directory explicitly to pin the interleaving where a path lookup resolved
// the old owned directory before a restore exchanged and reclaimed it.
// Atomic entry replacement gives no snapshot lifetime to that traversal.
func TestMaterializeTaskOutput_ReclaimsRetiredOwnedDirectory(t *testing.T) {
	forEachSwapPath(t, func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		owned := map[string]string{"sub/x.js": "x"}
		entry := ingestTree(t, s, "retired-directory", owned)
		dest := filepath.Join(t.TempDir(), "gen")
		writeTree(t, dest, map[string]string{
			"sub/x.js": "x", "migration-bundle/bundle.json": "bundle",
		})
		ceded := filepath.Join(dest, "migration-bundle")
		untouched := snapshotInodes(t, ceded)
		held, err := os.OpenRoot(filepath.Join(dest, "sub"))
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		if data, err := held.ReadFile("x.js"); err != nil || string(data) != "x" {
			t.Fatalf("before restore: held directory read = %q, %v", data, err)
		}

		if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "migration-bundle"); err != nil {
			t.Fatal(err)
		}
		if _, err := held.ReadFile("x.js"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("retired directory descendant = %v, want reclaimed entry", err)
		}
		assertTree(t, dest, map[string]string{
			"sub/x.js": "x", "migration-bundle/bundle.json": "bundle",
		})
		assertSameInodes(t, ceded, untouched)
		assertNothingBeside(t, dest)
	})
}

// TestMaterializeTaskOutput_FileIsPublishedByRename pins the macOS
// running-executable rule: the primitive must never write over the live
// destination inode, because overwriting a running binary's bytes poisons the
// code-signing vnode cache. Proof is inode-level — a reader holding the old file
// open keeps seeing the old bytes, and the path resolves to a new inode.
func TestMaterializeTaskOutput_FileIsPublishedByRename(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()
	tool := DeclaredEntryOutput{ID: "tool", Kind: proto.OutputKindFile, Root: proto.OutputRootProject, Path: "bin/tool"}
	stage(t, staging, tool, "", "#!/bin/sh\nnew\n")
	if err := os.Chmod(TaskStagingPath(staging, tool), 0o755); err != nil {
		t.Fatal(err)
	}
	entry, err := s.IngestTaskEntry(staging, taskSpec("binary-key", tool))
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(dest, []byte("#!/bin/sh\nold\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A "running" consumer holding the old inode open.
	held, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	oldIno := inodeOf(t, dest)

	if _, err := s.MaterializeTaskOutput(entry, "tool", dest, ""); err != nil {
		t.Fatalf("materialize file output: %v", err)
	}

	if newIno := inodeOf(t, dest); newIno == oldIno {
		t.Error("destination inode unchanged: the bytes were written in place, not staged and renamed")
	}
	buf := make([]byte, 64)
	n, _ := held.Read(buf)
	if !strings.Contains(string(buf[:n]), "old") {
		t.Errorf("the held inode was rewritten: %q", buf[:n])
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "#!/bin/sh\nnew\n" {
		t.Errorf("destination = %q (err=%v), want the new bytes", data, err)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("destination mode = %v, want 0755 (the exec bit must survive)", info.Mode().Perm())
	}
}

// TestMaterializeTaskOutput_DirectoryKeepsItsMode pins the mode of the restored
// directory ITSELF, not just of the files inside it. The staged replacement is
// created by os.MkdirTemp, which is 0700 by construction; without an explicit
// chmod the restored tree is owner-only where a fresh run leaves it 0755 — a
// difference that survives the build and reaches anything else reading the tree
// (another uid on CI, a docker build context, a published package).
func TestMaterializeTaskOutput_DirectoryKeepsItsMode(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()
	lib := DeclaredEntryOutput{ID: "lib", Kind: proto.OutputKindDirectory, Root: proto.OutputRootProject, Path: "lib"}
	stage(t, staging, lib, "index.js", "built")
	if err := os.Chmod(TaskStagingPath(staging, lib), 0o755); err != nil {
		t.Fatal(err)
	}
	entry, err := s.IngestTaskEntry(staging, taskSpec("dir-mode-key", lib))
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "lib")
	if _, err := s.MaterializeTaskOutput(entry, "lib", dest, ""); err != nil {
		t.Fatalf("materialize directory output: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("restored directory mode = %v, want 0755", info.Mode().Perm())
	}
}

// TestMaterializeTaskOutput_CedingMergeTakesTheStagedRootMode pins the mode
// half of the ceding merge. The destination directory keeps its inode, so it
// does not inherit the staged root's mode the way a swapped tree does; the
// merge has to set it, or a restore leaves the tree with whatever mode it had
// where a fresh run leaves the source's (see DirectoryKeepsItsMode).
func TestMaterializeTaskOutput_CedingMergeTakesTheStagedRootMode(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()
	gen := DeclaredEntryOutput{ID: "gen", Kind: proto.OutputKindDirectory, Root: proto.OutputRootProject, Path: ".gen"}
	stage(t, staging, gen, "generate-result.json", "new")
	if err := os.Chmod(TaskStagingPath(staging, gen), 0o755); err != nil {
		t.Fatal(err)
	}
	entry, err := s.IngestTaskEntry(staging, taskSpec("merge-mode-key", gen))
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), ".gen")
	writeTree(t, dest, map[string]string{"migration-bundle/bundle.json": "bundle"})
	if err := os.Chmod(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	ino := inodeOf(t, dest)

	if _, err := s.MaterializeTaskOutput(entry, "gen", dest, "", "migration-bundle"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("merged directory mode = %v, want 0755", info.Mode().Perm())
	}
	if inodeOf(t, dest) != ino {
		t.Error("the destination was swapped, not merged: a ceding restore must keep its inode")
	}
}
