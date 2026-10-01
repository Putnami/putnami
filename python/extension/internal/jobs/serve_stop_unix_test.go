//go:build unix

package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/jsonl"
)

// A server run without watch mode that received the relayed stop is given the
// time its shutdown takes: it is not killed once the watch-restart grace runs
// out.
func TestRunServer_WaitsForTheServerAfterTheRelayedStop(t *testing.T) {
	original := serverStopGrace
	t.Cleanup(func() { serverStopGrace = original })
	serverStopGrace = 10 * time.Millisecond

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	dir := t.TempDir()
	env := append(os.Environ(), stopHelperEnv+"=1", stopHelperOutliveDirEnv+"="+dir)
	sigChan := make(chan os.Signal, 1)
	returned := make(chan int, 1)
	go func() {
		returned <- runServer(jsonl.New(), []string{executable, "-test.run=^TestServeStopHelperProcess$"}, t.TempDir(), env, sigChan)
	}()
	if !waitForFile(filepath.Join(dir, "ready"), 30*time.Second) {
		t.Fatal("the server never became ready")
	}
	sigChan <- syscall.SIGTERM

	select {
	case code := <-returned:
		if code != 0 {
			t.Fatalf("runServer = %d, want 0 after a stop", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runServer did not return after the server exited")
	}
	if _, err := os.Stat(filepath.Join(dir, "done")); err != nil {
		t.Fatalf("the server was killed before its shutdown finished: %v", err)
	}
}

// waitForFile reports whether path exists within timeout.
func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
