package remotecache

import (
	"context"
	"io"
	"net/http"
)

// uploadProgressKey is the context key of the upload progress hook.
type uploadProgressKey struct{}

// WithUploadProgress returns a context under which the write path calls fn
// each time an upload moves forward: bytes of a blob were read to be
// compressed or sent, a blob PUT was answered, or a control exchange of the
// batched path succeeded. A caller that waits for uploads uses it to tell a
// slow upload from a stalled one.
//
// fn runs on the goroutines that upload, several at once, so it must be safe
// for concurrent use and must not block.
func WithUploadProgress(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, uploadProgressKey{}, fn)
}

func uploadProgressFrom(ctx context.Context) func() {
	fn, _ := ctx.Value(uploadProgressKey{}).(func())
	return fn
}

// reportUploadProgress calls the context's progress hook, when it has one.
func reportUploadProgress(ctx context.Context) {
	if fn := uploadProgressFrom(ctx); fn != nil {
		fn()
	}
}

// progressBody reports each read of a request entity that yields bytes.
type progressBody struct {
	io.ReadCloser
	report func()
}

func (p progressBody) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if n > 0 {
		p.report()
	}
	return n, err
}

// trackUpload makes the entity of req report its reads to the progress hook
// of the request's context, on the first send and on every replay. It must run
// after the request has its GetBody, so a replay stays possible. An empty
// entity is left as it is: net/http sends no body for http.NoBody, and a
// wrapper would hide that.
func trackUpload(req *http.Request) {
	report := uploadProgressFrom(req.Context())
	if report == nil || req.Body == nil || req.Body == http.NoBody {
		return
	}
	req.Body = progressBody{ReadCloser: req.Body, report: report}
	if reopen := req.GetBody; reopen != nil {
		req.GetBody = func() (io.ReadCloser, error) {
			body, err := reopen()
			if err != nil {
				return nil, err
			}
			return progressBody{ReadCloser: body, report: report}, nil
		}
	}
}
