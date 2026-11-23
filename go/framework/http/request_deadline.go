package http

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultWriteTimeout bounds a response when ServerConfig.WriteTimeout is left
// unset, mirroring the struct tag's documented default.
const defaultWriteTimeout = 30 * time.Second

// writeTimeout resolves the server write timeout. Zero means the documented
// default; a negative value disables it.
func (c ServerConfig) writeTimeout() time.Duration {
	if c.WriteTimeout == 0 {
		return defaultWriteTimeout
	}
	return c.WriteTimeout
}

// requestDeadline is how long a request runs before its context ends with
// context.DeadlineExceeded: nine tenths of the write timeout. A disabled write
// timeout sets no deadline.
func (c ServerConfig) requestDeadline() time.Duration {
	return deadlineWithin(c.writeTimeout())
}

// deadlineWithin is the request deadline under a write timeout: nine tenths of
// it. Over HTTP/2, a write timeout that fires first resets the stream and
// cancels the request context, so a stalled handler would read
// context.Canceled, the error a caller that left produces. The margin also
// leaves the handler time to write its 5xx answer.
func deadlineWithin(writeTimeout time.Duration) time.Duration {
	return writeTimeout - writeTimeout/10
}

// longLivedRequest reports a request whose response outlives the write timeout
// on purpose: a WebSocket upgrade, a Server-Sent Events stream, or a gRPC or
// Connect stream. Each clears the server write deadline and bounds its own
// writes, so a request deadline would cut it.
func longLivedRequest(r *http.Request) bool {
	if isWebSocketRequest(r) || acceptsEventStream(r) {
		return true
	}
	contentType := r.Header.Get("Content-Type")
	return hasPrefixFold(contentType, "application/grpc") ||
		hasPrefixFold(contentType, "application/connect+")
}

func hasPrefixFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

// requestContext is a request context that ends with
// context.DeadlineExceeded at its deadline and starts no timer until
// something waits on it. Deadline and Err answer from the deadline instant and
// the parent; the first Done, a lookup of the context package's cancellation
// key, the first read after an end, or a cancel arms the inner context every
// later call delegates to. Armed before its deadline, it ends as
// context.WithDeadline does; armed at or past its deadline, it ends with
// context.DeadlineExceeded even when its parent ended before the deadline,
// unless it was canceled before.
type requestContext struct {
	context.Context
	deadline time.Time

	inner    atomic.Pointer[armedContext]
	mu       sync.Mutex
	canceled bool
}

// armedContext is the context.WithDeadline a requestContext delegates to once
// armed.
type armedContext struct {
	context.Context
	cancel context.CancelFunc
}

// cancelCtxKey is the key under which the context package resolves the
// cancellation state of a context, as context.Cause asks for it. It is nil
// when the probe finds none.
var cancelCtxKey = probeCancelCtxKey()

// keyProbe is an ended context that records the first key a Value call asks
// for.
type keyProbe struct {
	context.Context
	key any
}

func (p *keyProbe) Err() error {
	return context.Canceled
}

func (p *keyProbe) Value(key any) any {
	if p.key == nil {
		p.key = key
	}
	return nil
}

// probeCancelCtxKey returns the key context.Cause looks up first: the
// context package's cancellation key.
func probeCancelCtxKey() any {
	probe := &keyProbe{Context: context.Background()}
	if context.Cause(probe) == nil {
		return nil
	}
	return probe.key
}

// newRequestContext returns a context that ends with
// context.DeadlineExceeded after timeout, or with the parent, as
// requestContext states. The caller cancels it with cancel once the request
// is done.
func newRequestContext(parent context.Context, timeout time.Duration) *requestContext {
	c := &requestContext{Context: parent, deadline: time.Now().Add(timeout)}
	if cur, ok := parent.Deadline(); ok && cur.Before(c.deadline) {
		c.deadline = cur
	}
	if cancelCtxKey == nil {
		c.arm()
	}
	return c
}

// arm returns the inner context, creating it on the first call.
func (c *requestContext) arm() *armedContext {
	if inner := c.inner.Load(); inner != nil {
		return inner
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armLocked()
}

// armLocked creates the inner context unless it exists. The caller holds c.mu.
// A context canceled while it still ran ends with context.Canceled, whatever
// its parent or its deadline do afterwards; one armed past its deadline ends
// with context.DeadlineExceeded, even when its parent ended too.
func (c *requestContext) armLocked() *armedContext {
	if inner := c.inner.Load(); inner != nil {
		return inner
	}
	var ctx context.Context
	var cancel context.CancelFunc
	switch {
	case c.canceled:
		ctx, cancel = context.WithCancel(context.WithoutCancel(c.Context))
		cancel()
	case !time.Now().Before(c.deadline):
		// Past its deadline, the context ends with context.DeadlineExceeded
		// whether or not its parent ended too.
		ctx, cancel = context.WithDeadline(context.WithoutCancel(c.Context), c.deadline)
	default:
		ctx, cancel = context.WithDeadline(c.Context, c.deadline)
	}
	inner := &armedContext{Context: ctx, cancel: cancel}
	c.inner.Store(inner)
	return inner
}

// cancel ends the context with context.Canceled, unless it already ended: a
// context past its deadline, or whose parent ended, keeps that end.
func (c *requestContext) cancel() {
	c.mu.Lock()
	inner := c.inner.Load()
	if inner == nil && (c.Context.Err() != nil || !time.Now().Before(c.deadline)) {
		inner = c.armLocked()
	}
	c.canceled = true
	c.mu.Unlock()
	if inner != nil {
		inner.cancel()
	}
}

// Deadline returns the instant the context ends with
// context.DeadlineExceeded.
func (c *requestContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

// Done arms the context and returns the channel closed when it ends.
func (c *requestContext) Done() <-chan struct{} {
	return c.arm().Done()
}

// Err returns why the context ended, or nil while it runs.
func (c *requestContext) Err() error {
	if inner := c.inner.Load(); inner != nil {
		return inner.Err()
	}
	c.mu.Lock()
	canceled := c.canceled
	c.mu.Unlock()
	if canceled || c.Context.Err() != nil || !time.Now().Before(c.deadline) {
		return c.arm().Err()
	}
	return nil
}

// Value arms the context for the context package's cancellation key, so a
// derived context and context.Cause see the armed one, and returns the
// parent's value for any other key.
func (c *requestContext) Value(key any) any {
	if key == cancelCtxKey {
		return c.arm().Value(key)
	}
	return c.Context.Value(key)
}
