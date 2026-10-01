// Package privatebroker reads the invocation-owned loopback publication broker
// a native publication run exports for one registry kind.
//
// Under a native publication run the CI runner holds the publication capability
// itself and starts a loopback broker in front of every registry. It exports
// one PUTNAMI_REGISTRY_<KIND>_URL per kind, each an HTTP numeric-loopback origin
// with an explicit port and the kind's path (`/oci`, `/go`, `/npm`, `/put`).
// A publisher that honors the broker sends every registry request to that
// origin and asks the credential seam about the BROKER host: the cloud answers
// a loopback broker host with the run's capability, whereas a request for the
// canonical registry host would try a user session the runner does not have.
//
// Absence is the laptop path: no variable, no broker, the publisher keeps its
// direct route. A remote HTTPS value is compatibility input for @putnami/cloud's
// own credential selection and is ignored here. A value that claims a local
// route but is malformed is configuration, so it fails closed instead of
// silently publishing to the remote host.
package privatebroker

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Endpoint is one loopback publication broker for one registry kind.
type Endpoint struct {
	// URL is the broker origin plus the kind path, e.g. http://127.0.0.1:41234/go.
	// Every registry request goes below it.
	URL string
	// Host is the broker's host:port, the host to ask the credential seam about.
	Host string
}

// FromEnv reads envName and returns the broker for kindPath (`/go`, `/npm`,
// `/oci`, `/put`). It returns (nil, nil) when the variable is unset or names an
// ordinary remote HTTPS endpoint, an Endpoint when it names a well-formed
// loopback broker, and an error when it claims a local route but is malformed.
func FromEnv(envName, kindPath string) (*Endpoint, error) {
	return Parse(os.Getenv(envName), kindPath)
}

// Parse applies the FromEnv rules to one raw value. kindPath is the exact path
// the broker mounts the registry kind at; any other path is refused.
func Parse(raw, kindPath string) (*Endpoint, error) {
	if raw == "" {
		return nil, nil
	}
	if strings.TrimSpace(raw) != raw {
		return nil, fmt.Errorf("private %s registry URL is invalid", kindName(kindPath))
	}
	broker, err := url.Parse(raw)
	if err != nil || broker.Host == "" {
		return nil, fmt.Errorf("private %s registry URL is invalid", kindName(kindPath))
	}
	ip := net.ParseIP(broker.Hostname())
	claimedLocal := strings.EqualFold(broker.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	// @putnami/cloud has long used these variables for its ordinary HTTPS
	// registry endpoints. The Framework did not consume those endpoints and
	// keeps ignoring a remote HTTPS value; only an invocation-owned numeric
	// loopback enables the private route.
	if broker.Scheme == "https" && !claimedLocal {
		return nil, nil
	}
	if broker.Scheme != "http" || broker.User != nil || broker.RawQuery != "" ||
		broker.Fragment != "" || broker.Path != kindPath || broker.RawPath != "" || broker.ForceQuery {
		return nil, fmt.Errorf("private %s registry URL is invalid", kindName(kindPath))
	}
	port, portErr := strconv.Atoi(broker.Port())
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("private %s registry URL must use an HTTP numeric loopback origin with an explicit port and %s path", kindName(kindPath), kindPath)
	}
	return &Endpoint{
		URL:  (&url.URL{Scheme: broker.Scheme, Host: broker.Host, Path: kindPath}).String(),
		Host: broker.Host,
	}, nil
}

func kindName(kindPath string) string {
	return strings.ToUpper(strings.TrimPrefix(kindPath, "/"))
}
