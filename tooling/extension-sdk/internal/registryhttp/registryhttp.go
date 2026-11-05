// Package registryhttp holds what the publishers share to reach a registry
// with a bearer: the registry URL check, and an HTTP client with no proxy, so
// the bearer has no second recipient, that never follows a redirect, so a
// request reaches the registry it names and no other.
package registryhttp

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/registryorigin"
)

// ValidateURL returns raw as an absolute HTTPS URL, or an HTTP URL on a
// loopback host, with no trailing slash and a path in its canonical escaping.
// A URL that carries userinfo, a query, an empty query or a fragment is
// refused, so no credential travels in a URL. label names the registry in
// every error, as in "put registry URL must use HTTP(S)"; no error carries
// the URL.
func ValidateURL(label, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", errors.New(label + " registry URL must be an absolute HTTP(S) URL without credentials")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New(label + " registry URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "https":
	case "http":
		hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		ip := net.ParseIP(hostname)
		loopback := hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || (ip != nil && ip.IsLoopback())
		if !loopback {
			return "", errors.New(label + " registry URL must use HTTPS (HTTP is allowed only for loopback)")
		}
	default:
		return "", errors.New(label + " registry URL must use HTTP(S)")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// NewClient returns the client a publication sends through: no proxy, no
// redirect, and a five-minute timeout. A registry host that origins maps is
// reached at its mapped origin (registryorigin); nil origins map none.
// transport names the client's transport in the error NewClient returns when
// the default transport is not an *http.Transport.
func NewClient(transport string, origins registryorigin.Origins) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New(transport + " is unavailable")
	}
	direct := base.Clone()
	direct.Proxy = nil
	return &http.Client{
		Timeout:       5 * time.Minute,
		Transport:     registryorigin.Transport(direct, origins),
		CheckRedirect: refuseRedirect,
	}, nil
}

// WithoutRedirects returns a copy of client that never follows a redirect, so
// a request carrying the bearer reaches the registry it names and no other.
// A nil client is an error that names the publication, as in "put
// publication requires an HTTP client".
func WithoutRedirects(client *http.Client, publication string) (*http.Client, error) {
	if client == nil {
		return nil, errors.New(publication + " requires an HTTP client")
	}
	bounded := *client
	bounded.CheckRedirect = refuseRedirect
	return &bounded, nil
}

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
