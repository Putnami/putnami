package storage

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// errUploadDeadline is the cause of a Put whose storage side did not move
// within the request timeout: it did not take the body bytes it was handed, or
// did not answer after the body ended. It wraps context.DeadlineExceeded, so a
// caller that retries a timeout recognizes it.
var errUploadDeadline = fmt.Errorf("storage: no storage progress within the request timeout: %w", context.DeadlineExceeded)

// uploadDeadline bounds the storage side of a Put, not the caller's reader.
// The body is the caller's reader, often a client's request stream, so a
// deadline from the start of Put would cover the client's upload time: a slow
// client that keeps sending would fail. Instead the deadline runs only while
// storage owes progress: from the moment the reader hands over bytes until the
// transport asks for more, and from the end of the body until Put returns.
// While the transport waits on the caller's reader, only the caller's context
// bounds the call.
type uploadDeadline struct {
	cancel  context.CancelCauseFunc
	timeout time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	ended   bool
	stopped bool
}

// withUploadDeadline returns a context for one Put's storage request and its
// deadline. A zero or negative timeout sets no deadline. Call stop when the
// Put returns.
func withUploadDeadline(ctx context.Context, timeout time.Duration) (context.Context, *uploadDeadline) {
	ctx, cancel := context.WithCancelCause(ctx)
	return ctx, &uploadDeadline{cancel: cancel, timeout: timeout}
}

// waiting pauses the deadline while the caller's reader produces bytes. It
// does nothing once the body has ended.
func (d *uploadDeadline) waiting() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended || d.timer == nil {
		return
	}
	d.timer.Stop()
}

// handed starts the deadline again: storage has the timeout to take the bytes
// it was handed or, once end is true, to answer. After the body has ended it
// does nothing, so the answer's deadline is never pushed back.
func (d *uploadDeadline) handed(end bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.ended || d.timeout <= 0 {
		return
	}
	d.ended = end
	if d.timer == nil {
		d.timer = time.AfterFunc(d.timeout, func() { d.cancel(errUploadDeadline) })
		return
	}
	d.timer.Reset(d.timeout)
}

// stop releases the deadline and its context.
func (d *uploadDeadline) stop() {
	d.mu.Lock()
	d.stopped = true
	if d.timer != nil {
		d.timer.Stop()
	}
	d.mu.Unlock()
	d.cancel(context.Canceled)
}

// cause returns errUploadDeadline when the deadline ended ctx, and err
// otherwise. The transport reports the context's cause for the request, but a
// read of the answer body can report context.Canceled, which a caller reads as
// its own cancellation.
func (d *uploadDeadline) cause(ctx context.Context, err error) error {
	if stderrors.Is(context.Cause(ctx), errUploadDeadline) {
		return errUploadDeadline
	}
	return err
}

// sentReader wraps a Put body so that its deadline runs only while storage
// owes progress: paused during each read of the caller's reader, started
// again when the read hands over bytes, and for the last time at the end of
// the body.
type sentReader struct {
	r        io.Reader
	deadline *uploadDeadline
}

func (s *sentReader) Read(p []byte) (int, error) {
	s.deadline.waiting()
	n, err := s.r.Read(p)
	switch {
	case stderrors.Is(err, io.EOF):
		s.deadline.handed(true)
	case n > 0:
		s.deadline.handed(false)
	}
	return n, err
}

// Close ends the body: the HTTP transport closes a request body once it has
// sent it, even when it read exactly ContentLength bytes and saw no EOF.
func (s *sentReader) Close() error {
	s.deadline.handed(true)
	return nil
}
