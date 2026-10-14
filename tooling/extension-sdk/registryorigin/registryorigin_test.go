package registryorigin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestParse_EmptyRoutesNothing(t *testing.T) {
	for _, raw := range []string{"", "  "} {
		origins, err := Parse(raw)
		if err != nil || origins != nil {
			t.Fatalf("Parse(%q) = %v, %v; want nil, nil", raw, origins, err)
		}
	}
}

func TestParse_ReadsPairs(t *testing.T) {
	origins, err := Parse(" OCI.example.dev=https://oci-1.example.net , put.example.dev:8443=https://put-1.example.net/")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(origins) != 2 {
		t.Fatalf("Parse read %d origins, want 2", len(origins))
	}
	if got := origins["oci.example.dev"].String(); got != "https://oci-1.example.net" {
		t.Errorf("oci origin = %s", got)
	}
	if got := origins["put.example.dev:8443"].String(); got != "https://put-1.example.net" {
		t.Errorf("put origin = %s", got)
	}
}

func TestParse_RefusesMalformedValues(t *testing.T) {
	for _, raw := range []string{
		"oci.example.dev",
		"=https://oci-1.example.net",
		"oci.example.dev/team=https://oci-1.example.net",
		"oci.example.dev=http://oci-1.example.net",
		"oci.example.dev=https://user:secret@oci-1.example.net",
		"oci.example.dev=https://oci-1.example.net/v2",
		"oci.example.dev=https://oci-1.example.net?token=secret",
		"oci.example.dev=https://oci-1.example.net#x",
		"oci.example.dev=https://",
		"oci.example.dev=https://oci.example.dev",
		"oci.example.dev=https://a.example.net,oci.example.dev=https://b.example.net",
	} {
		origins, err := Parse(raw)
		if err == nil {
			t.Errorf("Parse(%q) = %v, want an error", raw, origins)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("Parse(%q) error echoes the value: %v", raw, err)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(Env, "oci.example.dev=https://oci-1.example.net")
	origins, err := FromEnv()
	if err != nil || origins["oci.example.dev"] == nil {
		t.Fatalf("FromEnv = %v, %v", origins, err)
	}
	t.Setenv(Env, "")
	if origins, err := FromEnv(); err != nil || origins != nil {
		t.Fatalf("FromEnv with an empty value = %v, %v", origins, err)
	}
}

func TestTransport_EmptyKeepsBase(t *testing.T) {
	base := http.DefaultTransport
	if Transport(base, nil) != base {
		t.Fatal("Transport with no origins must return base")
	}
}

// recordingTransport answers every request with the configured response and
// records what it was sent.
type recordingTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	location string
}

func (r *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	header := http.Header{}
	if r.location != "" {
		header.Set("Location", r.location)
	}
	return &http.Response{StatusCode: http.StatusAccepted, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
}

func mustParse(t *testing.T, raw string) Origins {
	t.Helper()
	origins, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return origins
}

func TestTransport_RoutesMappedHostAndKeepsTheCallerView(t *testing.T) {
	base := &recordingTransport{location: "https://oci-1.example.net/v2/team/app/blobs/uploads/abc?state=1"}
	routed := Transport(base, mustParse(t, "oci.example.dev=https://oci-1.example.net"))

	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://oci.example.dev/v2/team/app/blobs/uploads/?mount=x", nil)
	request.Header.Set("Authorization", "Bearer token")
	response, err := routed.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	sent := base.requests[0]
	if sent.URL.String() != "https://oci-1.example.net/v2/team/app/blobs/uploads/?mount=x" || sent.Host != "oci-1.example.net" {
		t.Errorf("sent %s with Host %q", sent.URL, sent.Host)
	}
	if sent.Header.Get("Authorization") != "Bearer token" {
		t.Error("the routed request lost its headers")
	}
	if request.URL.Host != "oci.example.dev" {
		t.Error("the caller's request was modified")
	}
	if response.Request != request {
		t.Error("the response must carry the caller's request")
	}
	if got := response.Header.Get("Location"); got != "https://oci.example.dev/v2/team/app/blobs/uploads/abc?state=1" {
		t.Errorf("Location = %s", got)
	}
}

func TestTransport_KeepsOtherLocations(t *testing.T) {
	for _, location := range []string{"/v2/team/app/blobs/uploads/abc", "https://storage.example.net/blob"} {
		base := &recordingTransport{location: location}
		routed := Transport(base, mustParse(t, "oci.example.dev=https://oci-1.example.net"))
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://oci.example.dev/v2/", nil)
		response, err := routed.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		if got := response.Header.Get("Location"); got != location {
			t.Errorf("Location = %s, want %s", got, location)
		}
	}
}

func TestTransport_PassesUnmappedRequests(t *testing.T) {
	base := &recordingTransport{}
	routed := Transport(base, mustParse(t, "oci.example.dev=https://oci-1.example.net"))
	for _, raw := range []string{"https://ghcr.io/v2/", "http://oci.example.dev/v2/"} {
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
		if _, err := routed.RoundTrip(request); err != nil {
			t.Fatal(err)
		}
	}
	for index, sent := range base.requests {
		if sent.URL.Host != "ghcr.io" && sent.URL.Host != "oci.example.dev" {
			t.Errorf("request %d went to %s", index, sent.URL.Host)
		}
	}
}

func TestTransport_RefusesAMismatchedHostHeader(t *testing.T) {
	base := &recordingTransport{}
	routed := Transport(base, mustParse(t, "oci.example.dev=https://oci-1.example.net"))
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://oci.example.dev/v2/", nil)
	request.Host = "elsewhere.example.dev"
	if _, err := routed.RoundTrip(request); err == nil {
		t.Fatal("RoundTrip accepted a Host header that names another host")
	}
	if len(base.requests) != 0 {
		t.Fatal("a refused request reached the base transport")
	}
	if _, err := routed.RoundTrip(nil); err == nil {
		t.Fatal("RoundTrip accepted a nil request")
	}
}

func TestTransport_ReturnsBaseErrors(t *testing.T) {
	failing := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })
	routed := Transport(failing, mustParse(t, "oci.example.dev=https://oci-1.example.net"))
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://oci.example.dev/v2/", nil)
	if _, err := routed.RoundTrip(request); err == nil || err.Error() != "down" {
		t.Fatalf("RoundTrip error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// An image push names the public host and lands on the mapped origin: every
// request of the upload session, the bearer included, reaches the origin.
func TestTransport_PushesAnImageToTheOrigin(t *testing.T) {
	var mu sync.Mutex
	var hosts, authorizations []string
	handler := registry.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host)
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example.dev/token",service="oci.example.dev"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	origins := mustParse(t, "oci.example.dev="+server.URL)
	transport := Transport(server.Client().Transport, origins)

	image, err := random.Image(512, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference("oci.example.dev/team/app:v1", name.StrictValidation)
	if err != nil {
		t.Fatal(err)
	}
	auth := &authn.Bearer{Token: "publish-bearer"}
	if err := remote.Write(ref, image, remote.WithAuth(auth), remote.WithTransport(transport)); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
	pushed, err := remote.Head(ref, remote.WithAuth(auth), remote.WithTransport(transport))
	if err != nil {
		t.Fatalf("remote.Head: %v", err)
	}
	want, _ := image.Digest()
	if pushed.Digest != want {
		t.Errorf("registry holds %s, want %s", pushed.Digest, want)
	}

	origin := strings.TrimPrefix(server.URL, "https://")
	mu.Lock()
	defer mu.Unlock()
	authorized := 0
	for index, host := range hosts {
		if host != origin {
			t.Errorf("request %d reached host %s, want %s", index, host, origin)
		}
		if authorizations[index] == "Bearer publish-bearer" {
			authorized++
		}
	}
	if authorized == 0 {
		t.Error("no request carried the bearer")
	}
}
