package remotecache

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// A blob transfer carries the credential the cache server issued for one URL:
// a signature in the URL, or a storage token in an Authorization header. That
// credential is for the origin the server named and for no other.
//
// net/http alone does not keep it there. It drops Authorization when a redirect
// leaves the first host, but it keeps the header for a subdomain of that host,
// for another port of it, and for the same host over another scheme. The
// transfers therefore use their own redirect policy: the credential headers
// stay on a redirect inside the origin (scheme, host and port) and are removed
// on every other one.

// credentialHeaders are the request headers that carry a credential.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Cookie2"}

// maxTransferRedirects is net/http's own bound on the redirects of one request.
const maxTransferRedirects = 10

// transferClient returns the HTTP client of the blob transfers: a copy of hc,
// so both share one connection pool, whose redirect policy removes the
// credential headers from a redirect that leaves the transfer's origin. The
// policy of hc, when it has one, still decides whether the redirect is followed.
func transferClient(hc *http.Client) *http.Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	copied := *hc
	next := hc.CheckRedirect
	copied.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// net/http copies the first request's headers onto every hop before it
		// calls this, so the check runs, and removes them, on each hop.
		if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
			for _, name := range credentialHeaders {
				req.Header.Del(name)
			}
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= maxTransferRedirects {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &copied
}

// sameOrigin reports whether two URLs share a scheme, a host and a port.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		originPort(a) == originPort(b)
}

// originPort returns the port of u, or the default port of its scheme.
func originPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
