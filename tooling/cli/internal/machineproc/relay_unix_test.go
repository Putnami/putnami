//go:build linux || darwin

package machineproc

import (
	"os"
	"syscall"
	"testing"
)

// A relay's close returns only once its resize loop returned: the loop reads
// the master's and the client's descriptors, which close then closes and the
// client gives back (CLI ADR 0116 §4).
func TestARelayClosesOnlyOnceItsResizeLoopReturned(t *testing.T) {
	pipe := func() (*os.File, *os.File) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		return r, w
	}
	for range 100 {
		input, inputWriter := pipe()
		master, masterWriter := pipe()
		r := &terminalRelay{master: master, input: input, winch: make(chan os.Signal, 1)}
		r.watchSize()
		r.winch <- syscall.SIGWINCH
		r.close()
		select {
		case <-r.resized:
		default:
			t.Fatal("close returned while the resize loop still ran")
		}
		_ = inputWriter.Close()
		_ = masterWriter.Close()
	}
}
