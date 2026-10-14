// Package registryorigin sends the requests for a registry host to another
// origin that serves the same registry.
//
// A registry can be reachable at more than one address: its public name, and
// an address that a machine near the registry reaches without leaving the
// provider's network. The image references, the release coordinates and the
// publish credential all name the public host, so they stay as they are. Only
// the transport changes: a request for the public host goes to the origin
// named for it, with the same path, query and headers.
//
// PUTNAMI_REGISTRY_ORIGINS holds the map as comma-separated host=origin
// pairs, for example
//
//	oci.example.dev=https://oci-internal.example.net,put.example.dev=https://put-internal.example.net
//
// The host is a registry host as an image reference or a registry URL names
// it. The origin is an HTTPS URL with no path, query, fragment or user
// information. An unset or empty variable changes nothing. A malformed value
// is configuration, so it fails closed instead of silently keeping the public
// route.
package registryorigin

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Env names the variable that holds the map.
const Env = "PUTNAMI_REGISTRY_ORIGINS"

// Origins maps a registry host, lowercase, to the origin its requests go to.
// A nil or empty map routes nothing.
type Origins map[string]*url.URL

// FromEnv reads Env.
func FromEnv() (Origins, error) {
	return Parse(os.Getenv(Env))
}

// Parse reads one raw value of Env. Neither the error nor anything else it
// returns echoes the raw value.
func Parse(raw string) (Origins, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	origins := Origins{}
	for index, pair := range strings.Split(raw, ",") {
		host, origin, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			return nil, fmt.Errorf("%s entry %d is not host=origin", Env, index+1)
		}
		host = strings.ToLower(strings.TrimSpace(host))
		if !validHost(host) {
			return nil, fmt.Errorf("%s entry %d names no registry host", Env, index+1)
		}
		if _, seen := origins[host]; seen {
			return nil, fmt.Errorf("%s names %s twice", Env, host)
		}
		target, err := url.Parse(strings.TrimSpace(origin))
		if err != nil || target.Scheme != "https" || target.Opaque != "" || !validHost(strings.ToLower(target.Host)) ||
			target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" ||
			target.ForceQuery || target.Fragment != "" {
			return nil, fmt.Errorf("%s origin for %s is not an HTTPS origin without path, query or credentials", Env, host)
		}
		if strings.EqualFold(target.Host, host) {
			return nil, fmt.Errorf("%s routes %s to itself", Env, host)
		}
		origins[host] = &url.URL{Scheme: "https", Host: strings.ToLower(target.Host)}
	}
	return origins, nil
}

// validHost accepts a host name or host:port with no path, user information
// or whitespace.
func validHost(host string) bool {
	if host == "" || strings.ContainsAny(host, "/@?#\\ \t") {
		return false
	}
	parsed, err := url.Parse("https://" + host)
	return err == nil && parsed.Host == host && parsed.Hostname() != ""
}

// Transport returns base unchanged when origins is empty, and otherwise a
// transport that sends each HTTPS request for a mapped host to its origin.
//
// The caller keeps addressing the public host: its credential lookup, its
// redirect checks and the URLs it builds from a response all see the public
// host. The response's Request is the caller's request, and a Location header
// that names the origin is rewritten to name the public host, so the next
// request of an upload session comes back through this transport.
func Transport(base http.RoundTripper, origins Origins) http.RoundTripper {
	if len(origins) == 0 {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, origins: origins}
}

type transport struct {
	base    http.RoundTripper
	origins Origins
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("registry origin transport received no request")
	}
	if request.URL.Scheme != "https" {
		return t.base.RoundTrip(request)
	}
	host := strings.ToLower(request.URL.Host)
	origin, ok := t.origins[host]
	if !ok {
		return t.base.RoundTrip(request)
	}
	if request.Host != "" && !strings.EqualFold(request.Host, host) {
		return nil, fmt.Errorf("registry origin transport refused a request for %s that names host %s", host, request.Host)
	}

	routed := request.Clone(request.Context())
	routedURL := *request.URL
	routedURL.Host = origin.Host
	routed.URL = &routedURL
	routed.Host = origin.Host

	response, err := t.base.RoundTrip(routed)
	if err != nil {
		return nil, err
	}
	response.Request = request
	if location := response.Header.Get("Location"); location != "" {
		if parsed, err := url.Parse(location); err == nil && parsed.IsAbs() && strings.EqualFold(parsed.Host, origin.Host) {
			parsed.Scheme = request.URL.Scheme
			parsed.Host = request.URL.Host
			response.Header = response.Header.Clone()
			response.Header.Set("Location", parsed.String())
		}
	}
	return response, nil
}

var _ http.RoundTripper = (*transport)(nil)
