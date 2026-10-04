package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// An empty source state writes zero bytes, so every key computed inside a
// repository keeps its v8 address. The unmanaged state inserts exactly
// "sourceState\0unmanaged\0" after the OS-class block and before the task, and
// nothing else moves: the two states of one task differ by that marker alone.
func TestCacheKey_SourceStateAddsOnlyItsMarker(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "source-state-keys-the-cache",
		"an-unmanaged-root-adds-only-its-marker-to-the-key")
	cm := NewCacheManager(nil)

	managed, err := pinnedFormatKey().ComputeHashUsing(cm)
	if err != nil {
		t.Fatal(err)
	}
	if managed != pinnedPOSIXKeyHash {
		t.Fatalf("an empty source state moved the key: got %s, want %s", managed, pinnedPOSIXKeyHash)
	}

	unmanaged := pinnedFormatKey()
	unmanaged.SourceState = SourceStateUnmanaged
	got, err := unmanaged.ComputeHashUsing(cm)
	if err != nil {
		t.Fatal(err)
	}
	if want := pinnedFormatPreimageDigest("sourceState", "unmanaged"); got != want {
		t.Fatalf("the unmanaged key is not the repository preimage plus the sourceState marker: got %s, want %s", got, want)
	}
	if got == managed {
		t.Fatal("the unmanaged key equals the repository key")
	}

	windows := pinnedFormatKey()
	windows.OSClass = OSClassWindows
	windows.SourceState = SourceStateUnmanaged
	got, err = windows.ComputeHashUsing(cm)
	if err != nil {
		t.Fatal(err)
	}
	want := pinnedFormatPreimageDigest("osClass", "windows", "sourceState", "unmanaged")
	if got != want {
		t.Fatalf("the sourceState marker is not written after the OS class: got %s, want %s", got, want)
	}
}

// SourceState names the state of a root once per manager: empty inside a
// repository, with or without a commit, and unmanaged outside every one.
func TestCacheManager_SourceState(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "source-state-keys-the-cache",
		"an-unmanaged-root-adds-only-its-marker-to-the-key")
	parent := t.TempDir()
	// Git discovery stops at parent, so the answer does not depend on where
	// the temporary directory lives.
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)
	plain := filepath.Join(parent, "plain")
	repository := filepath.Join(parent, "repository")
	for _, dir := range []string{plain, repository} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", repository, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	cm := NewCacheManager(nil)
	if got := cm.SourceState(repository); got != "" {
		t.Fatalf("SourceState(repository) = %q, want empty", got)
	}
	if got := cm.SourceState(plain); got != SourceStateUnmanaged {
		t.Fatalf("SourceState(plain directory) = %q, want %q", got, SourceStateUnmanaged)
	}

	// The answer holds for the life of the manager: every key of one
	// invocation agrees even when the root changes state under it.
	if out, err := exec.Command("git", "-C", plain, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if got := cm.SourceState(plain); got != SourceStateUnmanaged {
		t.Fatalf("SourceState changed within one manager: %q", got)
	}
	if got := NewCacheManager(nil).SourceState(plain); got != "" {
		t.Fatalf("a new manager kept the previous state: %q", got)
	}
}

// A root a git command already answered for needs no second question: once
// RecordManagedRoot names it, SourceState answers empty without starting git.
// It never replaces an answer the manager holds, and a nil manager, which holds
// none, asks Git on every call.
func TestCacheManager_RecordManagedRoot(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "source-state-keys-the-cache",
		"the-key-and-the-stamp-read-one-answer")
	parent := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)
	plain := filepath.Join(parent, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	// No git program: any question SourceState asks answers unmanaged.
	t.Setenv("PATH", t.TempDir())

	recorded := NewCacheManager(nil)
	recorded.RecordManagedRoot(plain + string(filepath.Separator))
	if got := recorded.SourceState(plain); got != "" {
		t.Fatalf("SourceState of a recorded root = %q, want empty without asking git", got)
	}

	held := NewCacheManager(nil)
	if got := held.SourceState(plain); got != SourceStateUnmanaged {
		t.Fatalf("SourceState(plain directory) = %q, want %q", got, SourceStateUnmanaged)
	}
	held.RecordManagedRoot(plain)
	if got := held.SourceState(plain); got != SourceStateUnmanaged {
		t.Fatalf("RecordManagedRoot replaced the held answer: %q", got)
	}

	var none *CacheManager
	if got := none.SourceState(plain); got != SourceStateUnmanaged {
		t.Fatalf("a nil manager's SourceState = %q, want %q", got, SourceStateUnmanaged)
	}
}
