package observe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	readers "go.putnami.dev/evidence/readers"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

// Bounds of one xcrun command: its run time, and what it may print, the
// largest report a reader reads.
const (
	xcrunTimeout   = 5 * time.Minute
	xcrunMaxOutput = readers.MaxReportBytes
	// xcrunStderrBytes is how much of what a failed command printed on its
	// standard error its diagnostic quotes.
	xcrunStderrBytes = 512
	// xcrunWaitDelay is how long a stopped command's output may stay open.
	xcrunWaitDelay = 2 * time.Second
)

// systemXCRun returns the xcrun of the PATH, nil when the PATH has none, as
// on a Linux runner.
func systemXCRun(ctx context.Context) analyze.XCRun {
	program, err := exec.LookPath("xcrun")
	if err != nil {
		return nil
	}
	return func(args ...string) ([]byte, error) {
		return runBounded(ctx, xcrunTimeout, xcrunMaxOutput, program, args...)
	}
}

// errOutputTooLarge is a command that printed more than its bound.
var errOutputTooLarge = errors.New("the output is larger than the largest report observe reads")

// runBounded runs program with args, and returns its standard output. It
// stops the command past timeout, or once it printed more than limit bytes.
func runBounded(ctx context.Context, timeout time.Duration, limit int, program string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	// A child that keeps the output open once the command is stopped does
	// not hold the run past its bound.
	cmd.WaitDelay = xcrunWaitDelay
	stdout := &boundedBuffer{limit: limit, cancel: cancel}
	var stderr boundedBuffer
	stderr.limit = xcrunStderrBytes
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := cmd.Run()
	switch {
	case stdout.over:
		return nil, errOutputTooLarge
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("it did not finish within %s", timeout)
	case err != nil:
		if message := strings.TrimSpace(stderr.buf.String()); message != "" {
			return nil, fmt.Errorf("%w: %s", err, message)
		}
		return nil, err
	}
	return stdout.buf.Bytes(), nil
}

// boundedBuffer keeps at most limit bytes. A stdout buffer stops its command
// past them; a stderr buffer drops the rest.
type boundedBuffer struct {
	buf    bytes.Buffer
	limit  int
	over   bool
	cancel context.CancelFunc
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		if b.cancel != nil {
			b.over = true
			b.cancel()
			return 0, errOutputTooLarge
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}
