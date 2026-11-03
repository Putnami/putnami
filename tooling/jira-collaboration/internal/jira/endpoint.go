// Package jira is the Jira Cloud REST client of the Jira collaboration
// provider. It reads its credential from the environment, sends every request
// exactly once unless it is a read, and classifies every failure by what it
// proves about the remote state: a request that never left this process, one
// whose outcome is unknown, and a definite answer.
package jira

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// OverrideVariable names the environment variable that points the client at
// another API root. Only an http or https URL on the loopback interface is
// accepted, so the variable can reach a local stand-in and nothing else: the
// credential is never sent anywhere but the bound site or this machine.
const OverrideVariable = "PUTNAMI_JIRA_API_URL"

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(?::[0-9]{1,5})?$`)

// ValidHost reports whether host is a bare host name, optionally with a port:
// no scheme, path, query or user information.
func ValidHost(host string) bool {
	return hostPattern.MatchString(host)
}

// Endpoint is where one site's REST API answers.
type Endpoint struct {
	// Site is the Jira Cloud host the credential is sent to.
	Site string
	// REST is the REST API v2 root, without a trailing slash.
	REST *url.URL
}

// ResolveEndpoint returns the REST API v2 root of a site:
// https://<site>/rest/api/2, or the loopback root OverrideVariable names.
// getenv reads OverrideVariable.
func ResolveEndpoint(site string, getenv func(string) string) (Endpoint, error) {
	if !ValidHost(site) {
		return Endpoint{}, fmt.Errorf("site %q is not a bare host name", site)
	}
	root := "https://" + site
	if override := getenv(OverrideVariable); override != "" {
		parsed, err := url.Parse(override)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
			parsed.RawQuery != "" || parsed.Fragment != "" || !loopback(parsed.Hostname()) {
			return Endpoint{}, fmt.Errorf("%s must be an http or https URL on the loopback interface, without credentials or query", OverrideVariable)
		}
		root = strings.TrimSuffix(parsed.String(), "/")
	}
	rest, err := url.Parse(root + "/rest/api/2")
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse the REST root: %w", err)
	}
	return Endpoint{Site: site, REST: rest}, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Path builds a REST path from segments, escaping each one, so an issue id or
// a property key never changes the path's structure.
func Path(segments ...string) string {
	var builder strings.Builder
	for _, segment := range segments {
		builder.WriteByte('/')
		builder.WriteString(url.PathEscape(segment))
	}
	return builder.String()
}
