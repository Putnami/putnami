package layout

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

func TestLinkArtifactGlobal_AbsoluteTarget(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(t.TempDir(), "sha256", "ab", "abcd")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := LinkArtifactGlobal(ws, Extensions, "@putnami/go", target); err != nil {
		t.Fatal(err)
	}

	link := StableDir(ws, Extensions, "@putnami/go")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("global symlink target must be absolute, got %q", got)
	}
	if got != target {
		t.Errorf("target = %q, want %q", got, target)
	}
	// The link resolves to the real directory. dirlink.Resolve also follows
	// the junction the link is on Windows (D-W6), which filepath.EvalSymlinks
	// does not.
	resolved, err := dirlink.Resolve(link)
	if err != nil {
		t.Fatalf("resolve link: %v", err)
	}
	wantReal, _ := dirlink.Resolve(target)
	if resolved != wantReal {
		t.Errorf("resolved = %q, want %q", resolved, wantReal)
	}
}

func TestLinkArtifactGlobal_AtomicReplace(t *testing.T) {
	ws := t.TempDir()
	t1 := filepath.Join(t.TempDir(), "a")
	t2 := filepath.Join(t.TempDir(), "b")
	for _, d := range []string{t1, t2} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := LinkArtifactGlobal(ws, Extensions, "x", t1); err != nil {
		t.Fatal(err)
	}
	if err := LinkArtifactGlobal(ws, Extensions, "x", t2); err != nil {
		t.Fatal(err)
	}

	link := StableDir(ws, Extensions, "x")
	if got, _ := os.Readlink(link); got != t2 {
		t.Errorf("after replace, target = %q, want %q", got, t2)
	}
	if _, err := os.Lstat(link + ".tmp"); !os.IsNotExist(err) {
		t.Error("leftover .tmp symlink after swap")
	}
}

func TestUnlinkArtifact_LinkOnly(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(t.TempDir(), "t")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := LinkArtifactGlobal(ws, Extensions, "y", target); err != nil {
		t.Fatal(err)
	}

	if err := UnlinkArtifact(ws, Extensions, "y"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(StableDir(ws, Extensions, "y")); !os.IsNotExist(err) {
		t.Error("stable link should be removed")
	}
	// Unlink removes only the link — the shared target bytes must remain for
	// other worktrees.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target bytes must survive unlink: %v", err)
	}
	// Unlinking a missing link is not an error.
	if err := UnlinkArtifact(ws, Extensions, "y"); err != nil {
		t.Errorf("unlink of a missing link should be nil, got %v", err)
	}
}
