package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

func TestUncacheableArtifact(t *testing.T) {
	tests := []struct {
		rel  string
		want bool
	}{
		{"lcov.info.0.tmp", true},
		{"lcov.info.12.tmp", true},
		{filepath.Join("coverage", "lcov.info.3.tmp"), true},
		{"lcov.info", false},
		{"lcov.info.tmp", true}, // prefix "lcov.info." + suffix ".tmp" both match
		{"dist/index.js", false},
		{"report.tmp", false},
		{"lcov.info.0.tmp.bak", false},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			if got := uncacheableArtifact(tt.rel); got != tt.want {
				t.Errorf("uncacheableArtifact(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.txt")
	dstPath := filepath.Join(dir, "sub", "dst.txt")

	os.WriteFile(srcPath, []byte("hello"), 0o644)

	if err := copyFile(srcPath, dstPath); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q, want %q", data, "hello")
	}
}

func TestCopyDir(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")

	// Create source structure
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("bbb"), 0o644)

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	// Verify files exist
	data, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(data) != "aaa" {
		t.Errorf("a.txt: err=%v, data=%q", err, data)
	}
	data, err = os.ReadFile(filepath.Join(dst, "sub", "b.txt"))
	if err != nil || string(data) != "bbb" {
		t.Errorf("sub/b.txt: err=%v, data=%q", err, data)
	}
}

func TestMaterializeDirSymlinkPreservesTreeWithoutMutatingSource(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	srcBinary := filepath.Join(src, "bin", "tool")
	if err := os.WriteFile(srcBinary, []byte("cached binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	outputParent := t.TempDir()
	output := filepath.Join(outputParent, "build")
	if err := dirlink.Create(src, output); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeDirSymlink(output); err != nil {
		t.Fatalf("MaterializeDirSymlink: %v", err)
	}
	if info, err := os.Lstat(output); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("materialized output mode = %v, want real directory", info.Mode())
	}
	outputBinary := filepath.Join(output, "bin", "tool")
	if data, err := os.ReadFile(outputBinary); err != nil || string(data) != "cached binary\n" {
		t.Fatalf("materialized binary = %q, %v", data, err)
	}
	// Windows has no executable bit; a file's ".exe" name makes it runnable there.
	if info, err := os.Stat(outputBinary); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("materialized binary lost executable mode: %v", info.Mode())
	}

	if err := os.WriteFile(outputBinary, []byte("new binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(srcBinary); err != nil || string(data) != "cached binary\n" {
		t.Fatalf("detached write mutated cache source: %q, %v", data, err)
	}
}

func TestMaterializeDirSymlinkRemovesDanglingLink(t *testing.T) {
	output := filepath.Join(t.TempDir(), "build")
	if err := dirlink.Create(filepath.Join(t.TempDir(), "missing"), output); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeDirSymlink(output); err != nil {
		t.Fatalf("MaterializeDirSymlink: %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("dangling output still exists: %v", err)
	}
}

// The out/ links point into a machine-global store that GC and `putnami cache
// clean` may reclaim at any time; this reader holds no lease, so content can
// vanish after the link resolves but before the copy reads it. That is the same
// condition as a dangling link and must get the same handling — drop the link so
// the caller starts empty — rather than failing a build over a cache entry
// nobody promised to keep. Modeled here by content that disappears when read.
func TestMaterializeDirSymlinkTreatsVanishedContentAsDangling(t *testing.T) {
	blob := t.TempDir()
	if err := os.WriteFile(filepath.Join(blob, "artifact"), []byte("cached\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Present to the walk, gone to the read — what a mid-copy reclaim looks like.
	if err := dirlink.Create(filepath.Join(blob, "gone"), filepath.Join(blob, "reclaimed")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "build")
	if err := dirlink.Create(blob, output); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeDirSymlink(output); err != nil {
		t.Fatalf("reclaimed content must not fail the job: %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("stale link survived a reclaimed blob: %v", err)
	}
}

// Detaching must converge, not conflict. rename(2) cannot replace a symlink with
// a directory, so the swap is necessarily remove-then-rename, and a second
// process can land its own detach inside that window. Finding the path already
// converted is the end state this call wanted, so it must be reported as success
// — otherwise two putnami processes sharing a workspace fail each other's builds.
func TestMaterializeDirSymlinkConcurrentDetachConverges(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "artifact"), []byte("cached\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "build")
	if err := dirlink.Create(src, output); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = MaterializeDirSymlink(output)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d failed on an already-correct path: %v", i, err)
		}
	}
	if info, err := os.Lstat(output); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("output mode = %v, want real directory", info.Mode())
	}
	if data, err := os.ReadFile(filepath.Join(output, "artifact")); err != nil ||
		string(data) != "cached\n" {
		t.Fatalf("concurrent detach lost the tree: %q %v", data, err)
	}
}

// Resolving the link is not atomic with the Lstat that classified it, so a
// sibling landing its detach in that window makes readlink fail with EINVAL —
// the most likely way to lose this race, and one that must retry rather than
// fail a build. The test above only reaches that window by luck; this one pins
// the rule directly: a non-final attempt reports no resolve failure at all, and
// the final one reports it honestly so a genuinely broken path still surfaces.
// A self-referential link is a deterministic stand-in, since it fails to resolve
// with a non-ENOENT error exactly like the raced readlink does.
func TestMaterializeDirSymlinkRetriesUnresolvableLinkThenReports(t *testing.T) {
	output := filepath.Join(t.TempDir(), "build")
	if err := dirlink.Create(output, output); err != nil {
		t.Fatal(err)
	}

	done, err := materializeDirSymlinkOnce(output, false)
	if done || err != nil {
		t.Fatalf("non-final attempt = (%t, %v), want (false, nil) so the caller re-reads", done, err)
	}
	if done, err := materializeDirSymlinkOnce(output, true); done || err == nil {
		t.Fatalf("final attempt = (%t, %v), want a reported failure", done, err)
	}
	if err := MaterializeDirSymlink(output); err == nil {
		t.Fatal("MaterializeDirSymlink on a permanently unresolvable link = nil, want an error")
	}
	if _, err := os.Lstat(output); err != nil {
		t.Fatalf("a failed detach must leave the path as it found it: %v", err)
	}
}

// A real directory is left exactly as-is: this is the steady state after the
// first detach, and re-copying would clobber outputs a step just wrote.
func TestMaterializeDirSymlinkLeavesRealDirectoryUntouched(t *testing.T) {
	output := t.TempDir()
	fresh := filepath.Join(output, "just-built")
	if err := os.WriteFile(fresh, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeDirSymlink(output); err != nil {
		t.Fatalf("MaterializeDirSymlink: %v", err)
	}
	if data, err := os.ReadFile(fresh); err != nil || string(data) != "fresh\n" {
		t.Fatalf("real directory was disturbed: %q %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(output))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "tmp-materialize") {
			t.Fatalf("staging directory leaked: %s", e.Name())
		}
	}
}

// The detached copy must be writable even when the CAS blob directory is not,
// or the first write into the command output dir fails.
func TestMaterializeDirSymlinkDetachesReadOnlySource(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "artifact"), []byte("cached\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(src, 0o755) })

	output := filepath.Join(t.TempDir(), "build")
	if err := dirlink.Create(src, output); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeDirSymlink(output); err != nil {
		t.Fatalf("MaterializeDirSymlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(output, "new-artifact"), []byte("built\n"), 0o644); err != nil {
		t.Fatalf("detached output is not writable: %v", err)
	}
}

func TestBlobDir(t *testing.T) {
	s := &LocalStore{root: "/cache"}

	got := s.blobDir("abcdef123")
	want := filepath.Join("/cache", "blobs", "ab", "abcdef123")
	if got != want {
		t.Errorf("blobDir = %q, want %q", got, want)
	}
}

func TestBlobDirShortHash(t *testing.T) {
	s := &LocalStore{root: "/cache"}

	got := s.blobDir("a")
	want := filepath.Join("/cache", "blobs", "a", "a")
	if got != want {
		t.Errorf("blobDir(short) = %q, want %q", got, want)
	}
}
