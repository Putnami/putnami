package rust

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/jsonl"
)

// drainGrace is how long the output of a program may keep arriving after it
// exited. Tests shorten it.
var drainGrace = 2 * time.Second

// runForwarding runs cmd with its output on the job's log, since stdout
// carries the job's protocol: stdout as info, stderr as warnings. stderrCopy,
// when set, also receives stderr. The job owns the pipes rather than cmd, so a
// child the program leaves behind that holds them cannot keep the wait from
// returning.
func runForwarding(cmd *exec.Cmd, emit *jsonl.Emitter, stderrCopy io.Writer) error {
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return err
	}
	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite
	err = cmd.Start()
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
		return err
	}
	var stderr io.ReadCloser = stderrRead
	if stderrCopy != nil {
		stderr = io.NopCloser(io.TeeReader(stderrRead, stderrCopy))
	}
	var pipes sync.WaitGroup
	pipes.Add(2)
	go jsonl.ForwardPipe(emit, stdoutRead, "info", &pipes)
	go jsonl.ForwardPipe(emit, stderr, "warn", &pipes)
	err = cmd.Wait()
	forwarded := make(chan struct{})
	go func() {
		pipes.Wait()
		close(forwarded)
	}()
	select {
	case <-forwarded:
	case <-time.After(drainGrace):
	}
	// Closing the read ends unblocks a forwarder a leftover child holds.
	_ = stdoutRead.Close()
	_ = stderrRead.Close()
	<-forwarded
	return err
}
