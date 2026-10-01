package toolchain

import (
	"os"
	"strings"
	"time"
)

// goCacheRetryAttempts bounds the retries RunWithGoCacheRetry performs on top
// of the initial run, mirroring the lint batch's ETXTBSY handling.
const goCacheRetryAttempts = 3

// goCacheRetryDelay backs off between transient-GOCACHE retries; overridable
// in tests.
var goCacheRetryDelay = func(attempt int) time.Duration {
	return time.Duration(attempt+1) * 500 * time.Millisecond
}

// RunWithGoCacheRetry runs one Go toolchain invocation, retrying a bounded
// number of times when its combined output looks like a transient ENOENT on
// the machine-global Go build cache. That cache is deletable concurrently
// (cachepolicy.Clean removes <root>/build while other jobs point GOCACHE at
// it), so a publish-path subprocess can lose an entry mid-build; before this
// helper a human noticed the one-off failure and re-ran the project.
//
// The run callback must construct a fresh exec.Cmd per call: a started Cmd
// cannot be reused.
func RunWithGoCacheRetry(run func() ([]byte, error)) ([]byte, error) {
	var output []byte
	var err error
	for attempt := 0; attempt <= goCacheRetryAttempts; attempt++ {
		output, err = run()
		if err == nil {
			return output, nil
		}
		if attempt < goCacheRetryAttempts && isTransientGoCacheError(output) {
			time.Sleep(goCacheRetryDelay(attempt))
			continue
		}
		return output, err
	}
	return output, err
}

// isTransientGoCacheError recognizes the ENOENT class scoped to the shared Go
// cache root: a missing source file is a real failure and must not retry, so
// the path in the message has to reference the cache tree itself. The bare
// "GOCACHE" fallback covers messages that name the variable rather than a
// path.
func isTransientGoCacheError(output []byte) bool {
	message := string(output)
	if !strings.Contains(message, "no such file or directory") {
		return false
	}
	if root := ResolveGoCacheRoot(os.Getenv); root != "" && strings.Contains(message, root) {
		return true
	}
	return strings.Contains(message, "GOCACHE")
}
