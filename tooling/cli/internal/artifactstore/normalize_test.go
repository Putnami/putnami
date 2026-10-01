package artifactstore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // defeat the umask
		t.Fatal(err)
	}
}

// TestNormalizeTree_CollapsesModesAndTimes pins the two host-dependent
// properties extraction leaves behind: the wall clock, and the tar mode masked
// by the process umask.
func TestNormalizeTree_CollapsesModesAndTimes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "manifest.json"), `{"v":1}`, 0o600)
	writeFile(t, filepath.Join(root, "bin", "tool"), "#!/bin/sh\n", 0o777)
	if err := os.Chmod(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := NormalizeTree(root); err != nil {
		t.Fatalf("NormalizeTree: %v", err)
	}

	cases := []struct {
		rel  string
		mode os.FileMode
	}{
		{"manifest.json", 0o644},
		{"bin", 0o755 | os.ModeDir},
		{"bin/tool", 0o755},
	}
	for _, c := range cases {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(c.rel)))
		if err != nil {
			t.Fatal(err)
		}
		// Windows keeps only a read-only attribute, so the modes normalize on
		// Unix alone; the mtime normalizes everywhere.
		if runtime.GOOS != "windows" && info.Mode() != c.mode {
			t.Errorf("%s mode = %v, want %v", c.rel, info.Mode(), c.mode)
		}
		if !info.ModTime().UTC().Equal(NormalizedTime) {
			t.Errorf("%s mtime = %v, want %v", c.rel, info.ModTime().UTC(), NormalizedTime)
		}
	}

	// The root's own mtime is normalized; its mode belongs to the caller (in the
	// store it is the staging directory's, which stays tight).
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().UTC().Equal(NormalizedTime) {
		t.Errorf("root mtime = %v, want %v", info.ModTime().UTC(), NormalizedTime)
	}
}

// TestNormalizeTree_PreservesContent guards the one thing normalization must
// never touch: file bytes. The manifest's SHA-256 is the lock's
// platform-independent binding, re-checked on every cached link.
func TestNormalizeTree_PreservesContent(t *testing.T) {
	root := t.TempDir()
	const content = `{"name":"@putnami/test","version":"1.0.0"}`
	writeFile(t, filepath.Join(root, "putnami.extension.json"), content, 0o644)

	if err := NormalizeTree(root); err != nil {
		t.Fatalf("NormalizeTree: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("content = %q, want %q", got, content)
	}
}

// TestNormalizeTree_LeavesSymlinksAlone documents the deliberate gap: the
// standard library cannot set a link's own metadata, and following the link
// would rewrite its target instead.
func TestNormalizeTree_LeavesSymlinksAlone(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "target"), "x", 0o600)
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := NormalizeTree(root); err != nil {
		t.Fatalf("NormalizeTree: %v", err)
	}
	// Following the link must land on the NORMALIZED target, proving the walk did
	// not chase the symlink into rewriting it twice or skipping it.
	info, err := os.Stat(filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().UTC().Equal(NormalizedTime) {
		t.Errorf("link target mtime = %v, want %v", info.ModTime().UTC(), NormalizedTime)
	}
}

// TestStoreNormalize_StripsHostLocalBookkeeping pins what a packaging
// destination must NOT carry: staging, per-digest locks, the advisory flock, and
// the wall-clock recency sidecars.
func TestStoreNormalize_StripsHostLocalBookkeeping(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	const digest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	dir, err := s.Admit(digest, func(stageDir string) error {
		writeFile(t, filepath.Join(stageDir, "putnami.extension.json"), "{}", 0o600)
		return nil
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	// Admit stamps the recency sidecar and leaves the store scaffolding behind.
	if _, err := os.Stat(filepath.Join(dir, lastUsedFile)); err != nil {
		t.Fatalf("test setup: expected a lastused sidecar: %v", err)
	}

	if err := s.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	for _, leftover := range []string{tmpDirName, digestLocks, lockFile} {
		if _, err := os.Lstat(filepath.Join(root, leftover)); !os.IsNotExist(err) {
			t.Errorf("%s survived Normalize (err=%v)", leftover, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, lastUsedFile)); !os.IsNotExist(err) {
		t.Errorf("lastused survived Normalize (err=%v)", err)
	}

	// Everything that remains, including the store's own shard directories, is
	// stamped with the fixed time — the digest tree is the packaging output.
	for _, p := range []string{root, filepath.Join(root, shaDirName), filepath.Join(root, shaDirName, digest[:2]), dir, filepath.Join(dir, "putnami.extension.json")} {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().UTC().Equal(NormalizedTime) {
			t.Errorf("%s mtime = %v, want %v", p, info.ModTime().UTC(), NormalizedTime)
		}
	}
}

// TestStoreNormalize_IsIdempotent keeps re-running a materialization from
// churning the tree a caller hashes.
func TestStoreNormalize_IsIdempotent(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := s.Admit(digest, func(stageDir string) error {
		writeFile(t, filepath.Join(stageDir, "putnami.extension.json"), "{}", 0o644)
		return nil
	}); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	if err := s.Normalize(); err != nil {
		t.Fatalf("first Normalize: %v", err)
	}
	before := snapshotModes(t, root)
	if err := s.Normalize(); err != nil {
		t.Fatalf("second Normalize: %v", err)
	}
	if after := snapshotModes(t, root); after != before {
		t.Errorf("Normalize is not idempotent:\n%s\nvs\n%s", before, after)
	}
}

func snapshotModes(t *testing.T, root string) string {
	t.Helper()
	var out string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out += rel + " " + info.Mode().String() + " " + info.ModTime().UTC().Format(time.RFC3339Nano) + "\n"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
