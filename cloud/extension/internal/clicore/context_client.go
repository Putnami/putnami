package clicore

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// contextBoundClient returns a copy of client whose every request also ends
// when ctx ends. The copy keeps the client's own transport and timeout; client
// itself is never changed. A ctx that can never end returns client as is.
func contextBoundClient(ctx context.Context, client *http.Client) *http.Client {
	if ctx.Done() == nil {
		return client
	}
	bound := *WithUserAgent(client)
	bound.Transport = contextTransport{ctx: ctx, base: bound.Transport}
	return &bound
}

// contextTransport ends each request when the request's own context ends or
// when ctx ends, whichever comes first. A request made after ctx ended is
// never sent.
type contextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t contextTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithCancelCause(request.Context())
	stop := context.AfterFunc(t.ctx, func() { cancel(context.Cause(t.ctx)) })
	release := func() {
		stop()
		cancel(nil)
	}
	response, err := t.base.RoundTrip(request.WithContext(requestCtx))
	if err != nil {
		release()
		return nil, err
	}
	// The body is read under requestCtx, so it is released only when the
	// caller closes it.
	response.Body = &releasingBody{ReadCloser: response.Body, release: release}
	return response, nil
}

// releasingBody runs release once, after the body is closed.
type releasingBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
