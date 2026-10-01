package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func noRetryDelay(t *testing.T) {
	t.Helper()
	previous := goCacheRetryDelay
	goCacheRetryDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { goCacheRetryDelay = previous })
}

func TestRunWithGoCacheRetry_RetriesTransientCacheENOENT(t *testing.T) {
	noRetryDelay(t)
	calls := 0
	out, err := RunWithGoCacheRetry(func() ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("go: open GOCACHE entry: no such file or directory"), errors.New("exit status 1")
		}
		return []byte("ok"), nil
	})
	if err != nil {
		t.Fatalf("RunWithGoCacheRetry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want the transient failure retried once", calls)
	}
	if string(out) != "ok" {
		t.Fatalf("output = %q, want the successful run's output", out)
	}
}

func TestRunWithGoCacheRetry_DoesNotRetryOrdinaryFailures(t *testing.T) {
	noRetryDelay(t)
	calls := 0
	_, err := RunWithGoCacheRetry(func() ([]byte, error) {
		calls++
		// An ENOENT that references a source path, not the cache tree: a real
		// failure that retrying would only repeat.
		return []byte("open ./missing.go: no such file or directory"), errors.New("exit status 1")
	})
	if err == nil {
		t.Fatal("want the failure surfaced")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want no retry for a non-cache failure", calls)
	}
}

func TestRunWithGoCacheRetry_GivesUpAfterBoundedAttempts(t *testing.T) {
	noRetryDelay(t)
	calls := 0
	_, err := RunWithGoCacheRetry(func() ([]byte, error) {
		calls++
		return []byte("go: reading cache: GOCACHE: no such file or directory"), errors.New("exit status 1")
	})
	if err == nil {
		t.Fatal("want the persistent failure surfaced")
	}
	if calls != goCacheRetryAttempts+1 {
		t.Fatalf("calls = %d, want the initial run plus %d retries", calls, goCacheRetryAttempts)
	}
}

// TestRunWithGoCacheRetry_CoversTheHelperCacheTree keeps the retry honest once
// GOCACHEPROG is in force. The compiled objects then live in <root>/prog, which
// the cache-clean phase removes exactly like <root>/build, so an ENOENT naming
// a path in that tree is the same transient race the retry exists for. The
// resolved root covers it because the helper's tree sits under it.
func TestRunWithGoCacheRetry_CoversTheHelperCacheTree(t *testing.T) {
	noRetryDelay(t)
	root := ResolveGoCacheRoot(os.Getenv)
	if root == "" {
		t.Skip("no Go cache root resolves in this environment")
	}
	message := "open " + filepath.Join(root, "prog", "ab", "abcd-d") + ": no such file or directory"
	if !isTransientGoCacheError([]byte(message)) {
		t.Fatalf("an ENOENT under the helper cache tree is not recognized: %q", message)
	}
}

// TestRunWithGoCacheRetry_RecognizesAGoCacheProgFailure pins the message shape
// the go command produces when the helper binary itself is missing — which is
// what a concurrent extension reinstall looks like.
func TestRunWithGoCacheRetry_RecognizesAGoCacheProgFailure(t *testing.T) {
	message := `error starting GOCACHEPROG program "putnami-go": fork/exec putnami-go: no such file or directory`
	if !isTransientGoCacheError([]byte(message)) {
		t.Fatalf("a GOCACHEPROG startup ENOENT is not recognized: %q", message)
	}
}
