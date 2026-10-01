package artifactstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// hexDigest returns the 64-char lowercase-hex SHA-256 of s — the exact shape a
// real lock's Integrities["os/arch"] value has.
func hexDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func assertNoStaging(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.root, tmpDirName))
	if err != nil {
		return // no tmp dir at all is fine
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "staging-") {
			t.Errorf("leftover staging dir: %s", e.Name())
		}
	}
}

func TestPath_BareHexShardNoPrefix(t *testing.T) {
	s := New("/root")
	d := hexDigest("x")
	got := s.Path(d)
	want := filepath.Join("/root", "sha256", d[:2], d)
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	// The single most likely silent-miss bug: the lock stores BARE hex; the build
	// store's cas.go uses "sha256:"-prefixed. The artifact path must use neither a
	// prefix nor any colon.
	if strings.Contains(got, "sha256:") || strings.Contains(filepath.Base(got), ":") {
		t.Errorf("Path must use bare hex, got %q", got)
	}
}

func TestHasAndAdmit_RoundTrip(t *testing.T) {
	s := New(t.TempDir())
	d := hexDigest("ext-archive")
	if s.Has(d) {
		t.Fatal("digest should be absent before admit")
	}

	staged := false
	dir, err := s.Admit(d, func(stage string) error {
		staged = true
		return os.WriteFile(filepath.Join(stage, "bin"), []byte("hi"), 0o755)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !staged {
		t.Error("stage func was not invoked on a cold admit")
	}
	if dir != s.Path(d) {
		t.Errorf("admit dir = %q, want %q", dir, s.Path(d))
	}
	if !s.Has(d) {
		t.Error("Has is false after a successful admit")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bin")); string(b) != "hi" {
		t.Errorf("admitted content = %q, want %q", b, "hi")
	}
	// Digest fidelity: the literal lock value addresses the on-disk dir.
	if _, err := os.Stat(filepath.Join(s.root, "sha256", d[:2], d)); err != nil {
		t.Errorf("digest dir not at the bare-hex path: %v", err)
	}
}

func TestAdmit_HasShortCircuit_StageNotCalled(t *testing.T) {
	s := New(t.TempDir())
	d := hexDigest("present")
	if _, err := s.Admit(d, func(stage string) error {
		return os.WriteFile(filepath.Join(stage, "f"), []byte("a"), 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	// A second admit of the same digest must NOT re-run stage (no re-download).
	if _, err := s.Admit(d, func(stage string) error {
		t.Fatal("stage must not be called when the digest is already present")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdmit_VerifyFailure_NotPublished(t *testing.T) {
	s := New(t.TempDir())
	d := hexDigest("bad")
	wantErr := errors.New("verification failed")

	_, err := s.Admit(d, func(stage string) error {
		// Bytes are staged, but verification fails afterwards.
		_ = os.WriteFile(filepath.Join(stage, "partial"), []byte("x"), 0o644)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if s.Has(d) {
		t.Error("a digest must NOT be published when stage (verification) fails")
	}
	assertNoStaging(t, s)
}

func TestAdmit_StoreLockFailureDoesNotStage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(root)
	called := false
	if _, err := s.Admit(hexDigest("lock-failure"), func(string) error {
		called = true
		return nil
	}); err == nil {
		t.Fatal("Admit should fail when digest ownership cannot be acquired")
	}
	if called {
		t.Error("StageFunc ran without the required store lock")
	}
}

func TestWithSharedContext_CanceledBeforeFreeLockDoesNotRun(t *testing.T) {
	s := New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := s.WithSharedContext(ctx, func() { called = true })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithSharedContext error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("canceled bounded lock ran its callback")
	}
}

func TestWithSharedContext_ContentionHonorsDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-unix flock does not provide cross-process contention")
	}
	root := t.TempDir()
	s := New(root)
	if err := os.MkdirAll(root, dirPerm); err != nil {
		t.Fatal(err)
	}
	held, err := flock.Acquire(filepath.Join(root, lockFile), true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	called := false
	err = s.WithSharedContext(ctx, func() { called = true })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WithSharedContext error = %v, want deadline exceeded", err)
	}
	if called {
		t.Fatal("contended bounded lock ran its callback")
	}
}

func TestAdmit_RejectsInvalidDigest(t *testing.T) {
	s := New(t.TempDir())
	valid := hexDigest("x")
	bad := []string{
		"",
		"..",
		"../../etc/passwd",
		"sha256:" + valid,      // the prefixed form must be rejected
		strings.ToUpper(valid), // uppercase is not the lock's shape
		valid[:63],             // too short
		"zz" + valid[2:],       // non-hex chars
		"a/b/" + valid[4:],     // embedded separators
	}
	for _, d := range bad {
		if s.Has(d) {
			t.Errorf("Has(%q) should be false", d)
		}
		if _, err := s.Admit(d, func(string) error { return nil }); err == nil {
			t.Errorf("Admit(%q) should reject an invalid digest", d)
		}
	}
	// Nothing escaped the store root.
	if entries, _ := os.ReadDir(filepath.Join(s.root, "sha256")); len(entries) != 0 {
		t.Errorf("invalid digests created %d sha256 entries, want 0", len(entries))
	}
}

func TestAdmit_FirstWriterWins_Concurrent(t *testing.T) {
	s := New(t.TempDir())
	d := hexDigest("concurrent")
	const n = 12

	var wg sync.WaitGroup
	var stages int
	var stagesMu sync.Mutex
	dirs := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dirs[i], errs[i] = s.Admit(d, func(stage string) error {
				stagesMu.Lock()
				stages++
				stagesMu.Unlock()
				return os.WriteFile(filepath.Join(stage, "bin"), []byte("payload"), 0o755)
			})
		}(i)
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Errorf("admit %d: %v", i, errs[i])
		}
		if dirs[i] != s.Path(d) {
			t.Errorf("admit %d dir = %q, want %q", i, dirs[i], s.Path(d))
		}
	}
	if !s.Has(d) {
		t.Error("digest absent after concurrent admit")
	}
	if b, _ := os.ReadFile(filepath.Join(s.Path(d), "bin")); string(b) != "payload" {
		t.Errorf("content = %q, want payload", b)
	}
	if stages != 1 {
		t.Errorf("stage calls = %d, want exactly 1 for a shared digest", stages)
	}
	assertNoStaging(t, s)
}

func TestAdmit_OneStageAcrossProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock ownership is Unix-only")
	}
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "stages")
	digest := hexDigest("process-concurrent")
	const n = 6

	cmds := make([]*exec.Cmd, n)
	outputs := make([]bytes.Buffer, n)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestArtifactStoreAdmitHelper$")
		cmds[i].Stdout = &outputs[i]
		cmds[i].Stderr = &outputs[i]
		cmds[i].Env = append(os.Environ(),
			"PUTNAMI_ARTIFACTSTORE_HELPER=1",
			"PUTNAMI_ARTIFACTSTORE_ROOT="+root,
			"PUTNAMI_ARTIFACTSTORE_DIGEST="+digest,
			"PUTNAMI_ARTIFACTSTORE_MARKER="+marker,
		)
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper: %v: %s", err, outputs[i].String())
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(data))); got != 1 {
		t.Fatalf("cross-process StageFunc calls = %d, want 1", got)
	}
}

func TestArtifactStoreAdmitHelper(t *testing.T) {
	if os.Getenv("PUTNAMI_ARTIFACTSTORE_HELPER") != "1" {
		return
	}
	s := New(os.Getenv("PUTNAMI_ARTIFACTSTORE_ROOT"))
	_, err := s.Admit(os.Getenv("PUTNAMI_ARTIFACTSTORE_DIGEST"), func(stage string) error {
		marker, err := os.OpenFile(os.Getenv("PUTNAMI_ARTIFACTSTORE_MARKER"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if _, err := marker.WriteString("stage\n"); err != nil {
			_ = marker.Close()
			return err
		}
		if err := marker.Close(); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(stage, "bin"), []byte("payload"), 0o755)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdmit_DistinctDigestsDistinctDirs(t *testing.T) {
	// Archive non-determinism is harmless: two different archive digests get two
	// dirs, no false dedup.
	s := New(t.TempDir())
	d1, d2 := hexDigest("one"), hexDigest("two")
	dir1, _ := s.Admit(d1, func(string) error { return nil })
	dir2, _ := s.Admit(d2, func(string) error { return nil })
	if dir1 == dir2 {
		t.Error("distinct digests must not collide")
	}
	if !s.Has(d1) || !s.Has(d2) {
		t.Error("both digests should be present")
	}
}

func TestAdmit_DirPerm0700(t *testing.T) {
	s := New(t.TempDir())
	d := hexDigest("perm")
	dir, err := s.Admit(d, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Windows permission bits carry no access control: the store lives in the
	// user's profile, whose access control list already keeps other users out.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("digest dir perm = %o, want 0700 (exec'd-binary boundary)", fi.Mode().Perm())
	}
}

func TestAdmit_StampsLastUsed(t *testing.T) {
	s := New(t.TempDir())
	d := hexDigest("touch")
	dir, err := s.Admit(d, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, lastUsedFile)); err != nil {
		t.Errorf("admit should stamp %s for GC recency: %v", lastUsedFile, err)
	}
}
