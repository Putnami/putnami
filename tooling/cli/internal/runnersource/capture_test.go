package runnersource

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
)

func sourceFixture(t *testing.T) (string, *Store) {
	t.Helper()
	repo := t.TempDir()
	gitCmd(t, repo, "init", "--initial-branch=main")
	writeSource(t, repo, ".gitignore", []byte("ignored\n"), 0o644)
	writeSource(t, repo, "tracked", []byte("committed"), 0o644)
	writeSource(t, repo, "deleted", []byte("delete me"), 0o644)
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-m", "fixture")
	store, err := OpenStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return repo, store
}

func gitCmd(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeSource(t *testing.T, root, name string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), data, mode); err != nil {
		t.Fatal(err)
	}
}

func captureSource(t *testing.T, store *Store, repo string) Snapshot {
	t.Helper()
	snapshot, err := store.Capture(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestSourceCaptureUsesWorktreeBytesAndSurvivesLaterEdits(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "immutable-source", "captured-worktree-bytes-survive-later-edits")
	repo, store := sourceFixture(t)
	writeSource(t, repo, "tracked", []byte("staged"), 0o644)
	gitCmd(t, repo, "add", "tracked")
	writeSource(t, repo, "tracked", []byte("unstaged bytes"), 0o644)
	writeSource(t, repo, "binary", []byte{0, 255, 128, '\n'}, 0o644)
	writeSource(t, repo, "ignored", []byte("private ambient input"), 0o644)
	writeSource(t, repo, "run.sh", []byte("#!/bin/sh\ncat tracked\n"), 0o755)
	if err := os.Remove(filepath.Join(repo, "deleted")); err != nil {
		t.Fatal(err)
	}
	// A runner on Windows refuses a symbolic link by design
	// (TestMaterializeRefusesSymlinkOnWindows), so the tree there has none.
	if runtime.GOOS != "windows" {
		if err := os.Symlink("tracked", filepath.Join(repo, "link")); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := captureSource(t, store, repo)
	if !snapshot.Git.Dirty || snapshot.Git.Head == "" {
		t.Fatalf("Git context lost dirty/index state: %+v", snapshot)
	}
	writeSource(t, repo, "tracked", []byte("later edits"), 0o644)
	dir, err := store.Materialize(t.TempDir(), snapshot.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git", "ignored", "deleted"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("materialized excluded %s: %v", name, err)
		}
	}
	wants := map[string][]byte{"tracked": []byte("unstaged bytes"), "binary": {0, 255, 128, '\n'}, "link": []byte("unstaged bytes")}
	if runtime.GOOS == "windows" {
		delete(wants, "link")
	}
	for name, want := range wants {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
	if runtime.GOOS == "windows" {
		return // Windows has no executable bit and cannot run a #! script.
	}
	cmd := exec.CommandContext(context.Background(), filepath.Join(dir, "run.sh"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "unstaged bytes" {
		t.Fatalf("real materialized subprocess = %q (%v)", out, err)
	}
}

func TestSourceDigestIsIndependentOfHeadAndIndex(t *testing.T) {
	repo, store := sourceFixture(t)
	writeSource(t, repo, "tracked", []byte("changed"), 0o644)
	dirty := captureSource(t, store, repo)
	gitCmd(t, repo, "add", "tracked")
	staged := captureSource(t, store, repo)
	if staged.Digest != dirty.Digest || staged.IndexDigest == dirty.IndexDigest {
		t.Fatalf("source and index domains were conflated")
	}
	gitCmd(t, repo, "commit", "-m", "record same bytes")
	clean := captureSource(t, store, repo)
	if clean.Digest != dirty.Digest || clean.Git.Head == dirty.Git.Head || clean.Git.Dirty {
		t.Fatalf("source identity moved with HEAD, or existing tree identity lost HEAD")
	}
}

func TestSourceCaptureRejectsConcurrentSourceOrIndexChange(t *testing.T) {
	for _, change := range []string{"source", "index", "new-file", "mode", "branch"} {
		t.Run(change, func(t *testing.T) {
			if change == "mode" && runtime.GOOS == "windows" {
				t.Skip("Windows has no executable bit: os.Chmod cannot make the mode change this case races")
			}
			repo, store := sourceFixture(t)
			writeSource(t, repo, "tracked", []byte("dirty before capture"), 0o644)
			_, err := store.capture(context.Background(), repo, nil, func() {
				switch change {
				case "source":
					writeSource(t, repo, "tracked", []byte("different dirty bytes"), 0o644)
				case "index":
					gitCmd(t, repo, "add", "tracked")
				case "new-file":
					writeSource(t, repo, "new", []byte("new"), 0o644)
				case "mode":
					if err := os.Chmod(filepath.Join(repo, "tracked"), 0o755); err != nil {
						t.Fatal(err)
					}
				case "branch":
					gitCmd(t, repo, "symbolic-ref", "HEAD", "refs/heads/other")
					gitCmd(t, repo, "update-ref", "HEAD", "refs/heads/main")
				}
			})
			if err == nil || !strings.Contains(err.Error(), "source changed during capture") {
				t.Fatalf("capture admitted concurrent %s change: %v", change, err)
			}
		})
	}
}

func TestSourceCaptureRejectsUnsupportedLayouts(t *testing.T) {
	for _, shape := range []string{"submodule", "assume-unchanged", "skip-worktree", "symlink-escape"} {
		t.Run(shape, func(t *testing.T) {
			repo, store := sourceFixture(t)
			switch shape {
			case "submodule":
				head := gitCmd(t, repo, "rev-parse", "HEAD")
				gitCmd(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+head+",submodule")
			case "assume-unchanged":
				gitCmd(t, repo, "update-index", "--assume-unchanged", "tracked")
			case "skip-worktree":
				gitCmd(t, repo, "update-index", "--skip-worktree", "tracked")
			case "symlink-escape":
				if err := os.Symlink("../outside", filepath.Join(repo, "escape")); err != nil {
					t.Fatal(err)
				}
			}
			_, err := store.Capture(context.Background(), repo, nil)
			if err == nil {
				t.Fatalf("capture admitted %s", shape)
			}
			// Refused for escaping, not for its spelling: Windows reads the
			// target back as ..\outside, which must not pass for a portability
			// error that hides where the link points.
			if shape == "symlink-escape" && !strings.Contains(err.Error(), "escapes the workspace") {
				t.Fatalf("an escaping link was refused for another reason: %v", err)
			}
		})
	}
}

// The executable bit of a tracked file survives capture on every host. Windows
// has no executable bit on disk, so the capture there reads it from the index;
// without that, one committed tree captured two different manifests.
func TestSourceCaptureKeepsATrackedExecutableMode(t *testing.T) {
	repo, store := sourceFixture(t)
	writeSource(t, repo, "run.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	gitCmd(t, repo, "add", "run.sh")
	gitCmd(t, repo, "update-index", "--chmod=+x", "run.sh")
	gitCmd(t, repo, "commit", "-m", "an executable")
	modes := map[string]string{}
	for _, entry := range captureSource(t, store, repo).Manifest.Entries {
		modes[entry.Path] = entry.Mode
	}
	if modes["run.sh"] != "0755" || modes["tracked"] != "0644" {
		t.Fatalf("captured modes = %v, want run.sh 0755 and tracked 0644", modes)
	}
}

func TestSourceStoreDeduplicatesAndRejectsCorruption(t *testing.T) {
	repo, store := sourceFixture(t)
	first := captureSource(t, store, repo)
	before, err := store.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := before.ReadDir(-1)
	_ = before.Close()
	if err != nil {
		t.Fatal(err)
	}
	_ = captureSource(t, store, repo)
	writeSource(t, repo, "tracked", []byte("one changed blob"), 0o644)
	_ = captureSource(t, store, repo)
	after, err := store.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	next, err := after.ReadDir(-1)
	_ = after.Close()
	if err != nil || len(next) != len(entries)+1 {
		t.Fatalf("changed snapshot added %d blobs, want 1 (%v)", len(next)-len(entries), err)
	}
	entry := first.Manifest.Entries[0]
	if err := store.root.WriteFile(strings.TrimPrefix(entry.Digest, "sha256:"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if _, err := store.Materialize(parent, first.Manifest); err == nil {
		t.Fatal("corrupt source was materialized")
	}
	children, err := os.ReadDir(parent)
	if err != nil || len(children) != 0 {
		t.Fatalf("failed materialization exposed partial source: %v (%v)", children, err)
	}
}

func TestSourceStoreConcurrentPublish(t *testing.T) {
	_, store := sourceFixture(t)
	data := []byte("concurrent immutable object")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			digest, err := store.put(bytes.NewReader(data), int64(len(data)))
			if err != nil || digest != runner.BlobDigest(data) {
				t.Errorf("publish %s: %v", digest, err)
			}
		})
	}
	wg.Wait()
}

func TestSourceStoreRejectsUnsafeInputs(t *testing.T) {
	_, store := sourceFixture(t)
	for _, size := range []int64{-1, 1, runner.MaxSourceFileBytes + 1} {
		if _, err := store.put(strings.NewReader("too long"), size); err == nil {
			t.Errorf("accepted incorrect blob size %d", size)
		}
	}
	if _, err := store.Materialize(t.TempDir(), runner.SourceManifest{Version: 99}); err == nil {
		t.Fatal("accepted invalid manifest")
	}
	if _, err := store.Capture(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("accepted non-repository source")
	}
	public := t.TempDir()
	if err := os.Chmod(public, 0o755); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenStore(public)
	if runtime.GOOS == "windows" {
		// Windows mode bits carry no access information; the profile ACL does.
		if err != nil {
			t.Fatalf("open source store on Windows: %v", err)
		}
		_ = opened.Close()
		return
	}
	if err == nil {
		_ = opened.Close()
		t.Fatal("opened public source store")
	}
}

func TestSourceGitOutputRemainsBoundedThroughIOCopy(t *testing.T) {
	var output boundedGitOutput
	_, err := io.Copy(&output, io.LimitReader(strings.NewReader(strings.Repeat("x", runner.MaxManifestBytes+1)), runner.MaxManifestBytes+1))
	if err == nil || output.buffer.Len() > runner.MaxManifestBytes {
		t.Fatal("Git metadata bypassed its byte limit through io.Copy")
	}
}

func TestSourceCaptureDetachedHeadAndCancellation(t *testing.T) {
	repo, store := sourceFixture(t)
	gitCmd(t, repo, "checkout", "--detach")
	if snapshot := captureSource(t, store, repo); snapshot.Git.Branch != "" {
		t.Fatalf("detached branch = %q", snapshot.Git.Branch)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Capture(ctx, repo, nil); err == nil {
		t.Fatal("canceled capture succeeded")
	}
}

func TestSourceCaptureUnbornHeadRetainsInitialBranch(t *testing.T) {
	repo := t.TempDir()
	gitCmd(t, repo, "init", "--initial-branch=initial")
	writeSource(t, repo, "uncommitted", []byte("first snapshot"), 0o644)
	store, err := OpenStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	snapshot := captureSource(t, store, repo)
	if snapshot.Git.Head != "" || snapshot.Git.Branch != "initial" || !snapshot.Git.Dirty {
		t.Fatalf("unborn Git context = %+v", snapshot.Git)
	}
	if len(snapshot.Manifest.Entries) != 1 || snapshot.Manifest.Entries[0].Path != "uncommitted" {
		t.Fatalf("unborn source manifest = %+v", snapshot.Manifest)
	}
}

func TestSourceDirectoriesRejectsUnseenExistingDestination(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir("filesystem-spelling", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sourceDirectories(root, "filesystem-spelling", map[string]bool{}); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Fatalf("unseen destination alias accepted: %v", err)
	}
}

func TestSourceMaterializationRejectsUnicodeNormalizationAlias(t *testing.T) {
	const composed = "\u00e9"
	const decomposed = "e\u0301"
	probe := t.TempDir()
	if err := os.Mkdir(filepath.Join(probe, decomposed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(probe, composed), 0o755); err == nil {
		t.Skip("destination filesystem distinguishes Unicode normalization forms")
	} else if !os.IsExist(err) {
		t.Fatalf("probe Unicode normalization behavior: %v", err)
	}

	_, store := sourceFixture(t)
	first := []byte("first")
	second := []byte("second")
	firstDigest, err := store.put(bytes.NewReader(first), int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := store.put(bytes.NewReader(second), int64(len(second)))
	if err != nil {
		t.Fatal(err)
	}
	manifest := runner.SourceManifest{Version: runner.SourceManifestVersion, Entries: []runner.SourceEntry{
		{Path: decomposed + "/a", Kind: "file", Digest: firstDigest, Size: int64(len(first)), Mode: "0644"},
		{Path: composed + "/b", Kind: "file", Digest: secondDigest, Size: int64(len(second)), Mode: "0644"},
	}}
	if _, err := store.Materialize(t.TempDir(), manifest); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Fatalf("Unicode-normalization alias was merged: %v", err)
	}
}

// The bound-input half of capture: a git-ignored path the admission
// bound is captured flagged, verified ignored, raced like every other entry,
// and never demoted silently.
func TestSourceCaptureBindsIgnoredInputsExplicitly(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "capture-binds-only-ignored-paths")
	repo, store := sourceFixture(t)
	writeSource(t, repo, "ignored", []byte("private ambient input"), 0o644)
	if err := os.MkdirAll(filepath.Join(repo, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../ignored", filepath.Join(repo, "conf", "ignored-link")); err != nil {
		t.Fatal(err)
	}
	writeSource(t, repo, ".gitignore", []byte("ignored\nignored-link\n"), 0o644)
	gitCmd(t, repo, "add", ".gitignore")
	gitCmd(t, repo, "commit", "-m", "ignore the link too")

	unbound := captureSource(t, store, repo)
	snapshot, err := store.Capture(context.Background(), repo, []string{"ignored", "conf/ignored-link"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Digest == unbound.Digest {
		t.Fatal("binding an ignored input did not change the source identity")
	}
	bound := map[string]runner.SourceEntry{}
	for _, entry := range snapshot.Manifest.Entries {
		if entry.Bound {
			bound[entry.Path] = entry
		}
	}
	if len(bound) != 2 || bound["ignored"].Kind != "file" || bound["ignored"].Size != int64(len("private ambient input")) || bound["conf/ignored-link"].Kind != "symlink" || bound["conf/ignored-link"].Target != "../ignored" {
		t.Fatalf("bound entries = %+v", bound)
	}
	if runtime.GOOS == "windows" {
		t.Skip("a runner on Windows refuses a symbolic link by design (TestMaterializeRefusesSymlinkOnWindows)")
	}
	dir, err := store.Materialize(t.TempDir(), snapshot.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "ignored")); err != nil || string(got) != "private ambient input" {
		t.Fatalf("bound file = %q (%v)", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "conf", "ignored-link")); err != nil || string(got) != "private ambient input" {
		t.Fatalf("bound symlink did not resolve to the bound file: %q (%v)", got, err)
	}
	// The same tree with the same bound set has the same identity: binding is
	// deterministic, not a per-capture observation.
	again, err := store.Capture(context.Background(), repo, []string{"conf/ignored-link", "ignored"})
	if err != nil || again.Digest != snapshot.Digest {
		t.Fatalf("bound capture is not deterministic: %s vs %s (%v)", again.Digest, snapshot.Digest, err)
	}
}

func TestSourceCaptureRefusesABoundPathGitDoesNotIgnore(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "capture-binds-only-ignored-paths")
	repo, store := sourceFixture(t)
	writeSource(t, repo, "ignored", []byte("private"), 0o644)
	writeSource(t, repo, "untracked", []byte("new"), 0o644)
	for name, bound := range map[string][]string{
		"tracked":   {"tracked"},
		"untracked": {"untracked"},
		"absent":    {"ignored", "missing"},
		"git":       {".git/config"},
		"escape":    {"../outside"},
		"directory": {"."},
	} {
		if _, err := store.Capture(context.Background(), repo, bound); err == nil {
			t.Errorf("%s: bound %v was admitted", name, bound)
		}
	}
	if err := os.Mkdir(filepath.Join(repo, "ignored-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSource(t, repo, ".gitignore", []byte("ignored\nignored-dir/\n"), 0o644)
	if _, err := store.Capture(context.Background(), repo, []string{"ignored-dir"}); err == nil || !strings.Contains(err.Error(), "regular file or symlink") {
		t.Fatalf("a bound directory was admitted: %v", err)
	}
	if _, err := store.Capture(context.Background(), repo, []string{"ignored"}); err != nil {
		t.Fatalf("the dirty .gitignore did not stop the ignored path from binding: %v", err)
	}
	ignored, err := IgnoredPaths(context.Background(), repo, []string{"tracked", "untracked", "ignored", "ignored-dir/child", "missing"})
	if err != nil || len(ignored) != 2 || !ignored["ignored"] || !ignored["ignored-dir/child"] {
		t.Fatalf("IgnoredPaths = %v (%v)", ignored, err)
	}
	if ignored, err := IgnoredPaths(context.Background(), repo, []string{"tracked"}); err != nil || len(ignored) != 0 {
		t.Fatalf("a tracked path was reported ignored: %v (%v)", ignored, err)
	}
}

func TestSourceCaptureRacesABoundPathLikeAnyOther(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "input-admission", "capture-binds-only-ignored-paths")
	repo, store := sourceFixture(t)
	writeSource(t, repo, "ignored", []byte("before"), 0o644)
	for name, change := range map[string]func(){
		"edit":   func() { writeSource(t, repo, "ignored", []byte("after"), 0o644) },
		"delete": func() { _ = os.Remove(filepath.Join(repo, "ignored")) },
		"mode":   func() { _ = os.Chmod(filepath.Join(repo, "ignored"), 0o755) },
	} {
		if name == "mode" && runtime.GOOS == "windows" {
			// Windows has no executable bit: os.Chmod cannot make this mode change.
			continue
		}
		writeSource(t, repo, "ignored", []byte("before"), 0o644)
		_, err := store.capture(context.Background(), repo, []string{"ignored"}, change)
		if err == nil || !(strings.Contains(err.Error(), "source changed during capture") || strings.Contains(err.Error(), `capture "ignored"`)) {
			t.Errorf("%s of a bound input between scans was admitted: %v", name, err)
		}
	}
}
