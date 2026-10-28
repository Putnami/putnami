package useragent

import (
	"net/http"
	"strings"
	"sync/atomic"
)

const product = "putnami-cli"

var version atomic.Value

func init() {
	version.Store("dev")
}

// SetVersion sets the CLI version used in outbound HTTP User-Agent headers.
func SetVersion(v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		v = "dev"
	}
	version.Store(v)
}

// String returns the unified versioned User-Agent for Putnami CLI traffic.
func String() string {
	v, _ := version.Load().(string)
	return product + "/" + sanitizeToken(v)
}

// Set applies the putnami-cli/<version> User-Agent without overwriting a
// caller-selected one.
func Set(req *http.Request) {
	if req == nil {
		return
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", String())
	}
}

// NewTransport wraps base so requests sent through an http.Client get the
// unified User-Agent even if the caller uses helpers such as Client.Get.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return transport{base: base}
}

type transport struct {
	base http.RoundTripper
}

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") != "" {
		return t.base.RoundTrip(req)
	}
	cloned := req.Clone(req.Context())
	Set(cloned)
	return t.base.RoundTrip(cloned)
}

func sanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isTokenChar(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "dev"
	}
	return b.String()
}

func isTokenChar(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}
