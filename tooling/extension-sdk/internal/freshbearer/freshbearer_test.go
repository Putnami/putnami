package freshbearer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// recorder answers status and records the Authorization of each request.
type recorder struct {
	status int
	seen   []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = append(r.seen, req.Header.Get("Authorization"))
	return &http.Response{StatusCode: r.status, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func counting() Source {
	asks := 0
	return func(context.Context) (string, error) {
		asks++
		return fmt.Sprintf("token-%d", asks), nil
	}
}

func request(t *testing.T, target, authorization string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader("body"))
	req.RequestURI = ""
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return req
}

// Each request to the host with a bearer carries the source's value at that
// request, and every value sent is listed once.
func TestEachRequestCarriesTheSourcesCurrentBearer(t *testing.T) {
	base := &recorder{status: http.StatusOK}
	transport := New(base, "registry.test", counting())
	for range 2 {
		if _, err := transport.RoundTrip(request(t, "https://registry.test/v2/", "Bearer stale")); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"Bearer token-1", "Bearer token-2"}; !slices.Equal(base.seen, want) {
		t.Fatalf("sent %v, want %v", base.seen, want)
	}
	if want := []string{"token-1", "token-2"}; !slices.Equal(transport.Issued(), want) {
		t.Fatalf("issued %v, want %v", transport.Issued(), want)
	}
	if transport.Refused() {
		t.Fatal("a 200 answer marked the bearer refused")
	}
}

// A request to another host, or one without a bearer, goes through unchanged
// and never asks the source.
func TestOtherHostsAndAnonymousRequestsPassUnchanged(t *testing.T) {
	base := &recorder{status: http.StatusOK}
	asked := false
	transport := New(base, "registry.test", func(context.Context) (string, error) {
		asked = true
		return "token", nil
	})
	for _, req := range []*http.Request{
		request(t, "https://storage.test/blob", "Bearer other"),
		request(t, "https://registry.test/v2/", ""),
		request(t, "https://registry.test/v2/", "Basic dXNlcjpwYXNz"),
	} {
		if _, err := transport.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
	}
	if asked {
		t.Fatal("the source was asked for a request it does not authorize")
	}
	if want := []string{"Bearer other", "", "Basic dXNlcjpwYXNz"}; !slices.Equal(base.seen, want) {
		t.Fatalf("sent %v, want %v", base.seen, want)
	}
}

// A source error or an empty bearer ends the request before it is sent.
func TestASourceWithoutABearerEndsTheRequest(t *testing.T) {
	for name, source := range map[string]Source{
		"error": func(context.Context) (string, error) { return "", errors.New("renew failed") },
		"empty": func(context.Context) (string, error) { return "", nil },
	} {
		t.Run(name, func(t *testing.T) {
			base := &recorder{status: http.StatusOK}
			_, err := New(base, "registry.test", source).RoundTrip(request(t, "https://registry.test/v2/", "Bearer stale"))
			if err == nil || len(base.seen) != 0 {
				t.Fatalf("round trip = %v after %d sends; want an error and no send", err, len(base.seen))
			}
		})
	}
}

// A 401 or 403 answer marks the bearer refused, and Mark then makes the
// upload error match ErrRefused with its text kept.
func TestARefusedBearerMarksTheError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			transport := New(&recorder{status: status}, "registry.test", counting())
			if _, err := transport.RoundTrip(request(t, "https://registry.test/v2/", "Bearer stale")); err != nil {
				t.Fatal(err)
			}
			cause := &url.Error{Op: "Put", URL: "https://registry.test/v2/", Err: errors.New("denied")}
			marked := Mark(cause, transport)
			if !errors.Is(marked, ErrRefused) || marked.Error() != cause.Error() || !errors.As(marked, new(*url.Error)) {
				t.Fatalf("Mark = %v; want ErrRefused around the cause", marked)
			}
			if again := Mark(marked, transport); again != marked { //nolint:errorlint // identity: Mark returns marked itself, not a wrapper
				t.Fatal("Mark wrapped an error already marked")
			}
		})
	}
}

// Mark leaves an error alone when no bearer was refused.
func TestMarkKeepsAnErrorWithoutARefusal(t *testing.T) {
	cause := errors.New("network down")
	transport := New(&recorder{status: http.StatusOK}, "registry.test", counting())
	for _, tc := range []*Transport{nil, transport} {
		if got := Mark(cause, tc); got != cause { //nolint:errorlint // identity: Mark returns the cause itself
			t.Fatalf("Mark = %v, want the cause itself", got)
		}
	}
	if Mark(nil, transport) != nil {
		t.Fatal("Mark of no error returned an error")
	}
}

// A success after a refusal clears the mark: a client that refreshed and
// retried was accepted, so a later failure is not a refused bearer.
func TestASuccessAfterARefusalClearsTheMark(t *testing.T) {
	base := &recorder{status: http.StatusUnauthorized}
	transport := New(base, "registry.test", counting())
	if _, err := transport.RoundTrip(request(t, "https://registry.test/v2/", "Bearer stale")); err != nil || !transport.Refused() {
		t.Fatalf("a 401 left Refused = %t, %v", transport.Refused(), err)
	}
	base.status = http.StatusCreated
	if _, err := transport.RoundTrip(request(t, "https://registry.test/v2/", "Bearer stale")); err != nil || transport.Refused() {
		t.Fatalf("a 201 after the 401 left Refused = %t, %v", transport.Refused(), err)
	}
	if cause := errors.New("timeout"); Mark(cause, transport) != cause { //nolint:errorlint // identity: no mark after a success
		t.Fatal("Mark marked an error after the registry accepted a bearer")
	}
}

// Ask records the value it returns, so an error is redacted of a bearer no
// request carried.
func TestAskRecordsTheBearer(t *testing.T) {
	transport := New(&recorder{status: http.StatusOK}, "registry.test", counting())
	token, err := transport.Ask(context.Background())
	if err != nil || !slices.Equal(transport.Issued(), []string{token}) {
		t.Fatalf("Ask = %q, %v; issued %v", token, err, transport.Issued())
	}
}
