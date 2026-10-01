package dockerpublish

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"

	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/privatebroker"
)

const privateOCIRegistryURLEnv = "PUTNAMI_REGISTRY_OCI_URL"

// privateOCITransport keeps the managed OCI coordinate on every caller-facing
// request while sending its bytes through the invocation's loopback publication
// broker. It accepts one fixed logical host and one fixed local origin.
type privateOCITransport struct {
	logicalHost string
	broker      *url.URL
	base        http.RoundTripper
}

// privateOCITransportFor returns the per-publication transport and the host to
// ask the credential seam about. An absent environment value preserves the
// ordinary direct registry path. A remote HTTPS value remains compatibility
// input for Cloud's credential selection. A value claiming a local route is
// configuration, so an invalid local endpoint fails closed instead of silently
// publishing to the remote host.
func privateOCITransportFor(target imagePublishTarget) (http.RoundTripper, string, error) {
	broker, err := privatebroker.FromEnv(privateOCIRegistryURLEnv, "/oci")
	if err != nil {
		return nil, "", err
	}
	if broker == nil {
		return nil, target.Host, nil
	}
	if !target.Managed || target.Host != managedOCIRegistry {
		return nil, "", fmt.Errorf("private OCI registry URL requires the managed OCI target")
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, "", fmt.Errorf("private OCI registry transport unavailable")
	}
	localTransport := base.Clone()
	localTransport.Proxy = nil
	localTransport.ResponseHeaderTimeout = 5 * time.Minute
	return &privateOCITransport{
		logicalHost: target.Host,
		broker:      &url.URL{Scheme: "http", Host: broker.Host},
		base:        localTransport,
	}, broker.Host, nil
}

func (t *privateOCITransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.URL.Scheme != "https" ||
		request.URL.Host != t.logicalHost || request.URL.User != nil || request.URL.Fragment != "" ||
		request.URL.RawPath != "" || !validOCITransportPath(request.URL.Path) ||
		(request.Host != "" && request.Host != t.logicalHost) {
		return nil, fmt.Errorf("private OCI transport refused a non-canonical registry request")
	}

	localRequest := request.Clone(request.Context())
	localURL := *request.URL
	localURL.Scheme = t.broker.Scheme
	localURL.Host = t.broker.Host
	localURL.Path = "/oci" + request.URL.Path
	localRequest.URL = &localURL
	localRequest.Host = t.broker.Host

	response, err := t.base.RoundTrip(localRequest)
	if err != nil {
		return nil, fmt.Errorf("private OCI broker request failed: %w", err)
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		response.Body.Close()
		return nil, fmt.Errorf("private OCI broker redirect refused")
	}
	// go-containerregistry derives the registry scheme from Response.Request
	// after its unauthenticated ping. Preserve the logical HTTPS request here;
	// exposing the loopback hop would make its next request use HTTP for the
	// canonical host and would also leak a transport detail into upload state.
	response.Request = request
	location := response.Header.Get("Location")
	if location == "" {
		return response, nil
	}

	parsed, err := url.Parse(location)
	if err != nil {
		response.Body.Close()
		return nil, fmt.Errorf("private OCI broker Location refused")
	}
	resolved := localRequest.URL.ResolveReference(parsed)
	if resolved.Scheme != t.broker.Scheme || resolved.Host != t.broker.Host || resolved.User != nil ||
		resolved.Fragment != "" || resolved.RawPath != "" || !strings.HasPrefix(resolved.Path, "/oci/v2/") ||
		!validOCITransportPath(strings.TrimPrefix(resolved.Path, "/oci")) {
		response.Body.Close()
		return nil, fmt.Errorf("private OCI broker Location refused")
	}

	logical := url.URL{
		Scheme:   "https",
		Host:     t.logicalHost,
		Path:     strings.TrimPrefix(resolved.Path, "/oci"),
		RawQuery: resolved.RawQuery,
	}
	response.Header = response.Header.Clone()
	response.Header.Set("Location", logical.String())
	return response, nil
}

func validOCITransportPath(value string) bool {
	if !strings.HasPrefix(value, "/v2/") || strings.Contains(value, "//") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

var _ http.RoundTripper = (*privateOCITransport)(nil)

// dockerRoute is how one workload image publication reaches its registry: the
// host the credential seam is asked about, and the per-call transport when the
// invocation owns a private publication broker. A zero route is the direct
// registry path with default routing.
type dockerRoute struct {
	credentialHost string
	transport      http.RoundTripper
}

// resolveDockerRoute decides the route for a workload image publication.
//
// A managed registry (oci.putnami.dev) under a private loopback broker routes
// through the broker and asks the credential seam about the broker host: the
// cloud answers that host with the run's capability, whereas a request for the
// canonical host would try a user session this process does not have. Every
// other registry keeps the direct path. An empty registry pushes nothing and
// consults no route at all.
//
// The broker only carries manifest and blob requests, so a daemon-built
// candidate (no OCI layout, no digest) cannot be published through it; that is
// refused here rather than let `docker push` bypass the broker.
func resolveDockerRoute(registry string, manifest *pkgmeta.DockerManifest) (dockerRoute, error) {
	if registry == "" {
		return dockerRoute{}, nil
	}
	host := oci.RegistryHost(registry)
	transport, credentialHost, err := privateOCITransportFor(imagePublishTarget{
		Host:    host,
		Managed: host == managedOCIRegistry,
	})
	if err != nil {
		return dockerRoute{}, err
	}
	if transport != nil && (manifest.Layout == "" || manifest.Digest == "") {
		return dockerRoute{}, fmt.Errorf("private OCI registry broker requires an OCI layout candidate with a digest; repackage the image")
	}
	return dockerRoute{credentialHost: credentialHost, transport: transport}, nil
}

func (r dockerRoute) private() bool { return r.transport != nil }

func (r dockerRoute) pushOptions() []oci.PushOptions {
	if r.transport == nil {
		return nil
	}
	return []oci.PushOptions{{Transport: r.transport}}
}

func (r dockerRoute) craneOptions(keychain authn.Keychain) []crane.Option {
	options := []crane.Option{crane.WithAuthFromKeychain(keychain)}
	if r.transport != nil {
		options = append(options, crane.WithTransport(r.transport))
	}
	return options
}
