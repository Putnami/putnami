package distributioncli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.putnami.dev/client"
	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// putPublisher calls the put package protocol legs a publisher makes — blob
// upload, atomic publish, channel move and manifest readback — through
// put-server's generated client, forwarding the
// publisher's registry bearer on every call.
type putPublisher struct {
	put   *putserverclient.PutClient
	token string
}

// newPutPublisher binds put-server's generated client to the Put endpoint
// baseURL as given. The generated routes carry the `/put` mount, so each
// request URL is the one the hand-written publishers built from the same
// endpoint, the CI publication broker's `<broker>/put` included.
func newPutPublisher(base *http.Client, baseURL, token string) (*putPublisher, error) {
	put, err := clicore.NewServiceClient[putserverclient.PutClient](putserverclient.RegisterPutClient,
		clicore.ForwardedServiceBinding(baseURL, putLegHTTPClient(base), putServerReaderProfile))
	if err != nil {
		return nil, err
	}
	return &putPublisher{put: put, token: token}, nil
}

// call returns the context of one leg: the caller's context carrying the
// forwarded bearer, and the refusal put-server answers, if any.
func (p *putPublisher) call(ctx context.Context) (context.Context, *putRefusal) {
	refusal := &putRefusal{}
	ctx = context.WithValue(ctx, putRefusalKey{}, refusal)
	if p.token != "" {
		ctx = client.WithForwardedUserToken(ctx, p.token)
	}
	return ctx, refusal
}

// withUploadLength makes the upload ctx carries declare length as its
// Content-Length, as the hand-written upload did, instead of a chunked body.
func withUploadLength(ctx context.Context, length int64) context.Context {
	return context.WithValue(ctx, putUploadLengthKey{}, length)
}

type (
	putRefusalKey      struct{}
	putUploadLengthKey struct{}
)

// putRefusal holds the body of a refusal put-server answered. put-server
// writes `{"error": "<reason>"}`, which the generated client does not carry,
// so a publisher's error keeps put-server's own words this way.
type putRefusal struct{ body string }

// maxPutRefusalBytes bounds the refusal text a publisher keeps.
const maxPutRefusalBytes = 8 << 10

// putLegHTTPClient refuses every redirect, so a 307 or 308 never replays a
// registry bearer or a publish body, and leaves each call's bound to the
// operation's declared timeout.
func putLegHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	wrapped := *base
	wrapped.Timeout = 0
	wrapped.Transport = putLegTransport{base: base.Transport}
	wrapped.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &wrapped
}

// putLegTransport sets the Content-Length an upload's context declares and
// keeps the body of a refusal for the publisher's error. The generated client
// still reads the refusal: the transport hands it the same bytes.
type putLegTransport struct{ base http.RoundTripper }

func (t putLegTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if length, ok := request.Context().Value(putUploadLengthKey{}).(int64); ok && request.Body != nil && request.Body != http.NoBody {
		request = request.Clone(request.Context())
		request.ContentLength = length
	}
	response, err := base.RoundTrip(request)
	if err != nil || response.StatusCode < http.StatusBadRequest {
		return response, err
	}
	refusal, ok := request.Context().Value(putRefusalKey{}).(*putRefusal)
	if !ok {
		return response, nil
	}
	head, readErr := io.ReadAll(io.LimitReader(response.Body, maxPutRefusalBytes))
	refusal.body = strings.TrimSpace(string(head))
	response.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(head), response.Body), Closer: response.Body}
	if readErr != nil {
		refusal.body = ""
	}
	return response, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// putLegError renders a failed leg the way the hand-written publishers did: a
// refusal reads "<subject>: <status line>: <put-server's body>".
func putLegError(subject string, refusal *putRefusal, err error) error {
	if status := clicore.ServiceStatus(err); status != 0 {
		return clicore.NewError(fmt.Sprintf("%s: %s: %s", subject, clicore.StatusLine(status), refusal.body), clicore.ExitAPI)
	}
	return clicore.NewError(fmt.Sprintf("%s: %s", subject, clicore.ServiceFailureReason(err)), clicore.ExitAPI)
}

// invalidPutAnswer reports an answer outside put-server's contract.
func invalidPutAnswer(err error) bool {
	return err != nil && clicore.ServiceStatus(err) == 0 && clicore.ServiceFailureReason(err) == "invalid response"
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
