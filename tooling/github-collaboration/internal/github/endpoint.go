// Package github is the GitHub REST and GraphQL client of the GitHub
// collaboration provider. It resolves credentials the way the gh CLI finds
// them, sends every request exactly once unless it is a read, and classifies
// every failure by what it proves about the remote state: a request that never
// left this process, one whose outcome is unknown, and a definite answer.
package github

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// DefaultHost is the public GitHub host.
const DefaultHost = "github.com"

// OverrideVariable names the environment variable that points the client at
// another API root. Only an http or https URL on the loopback interface is
// accepted, so the variable can reach a local stand-in and nothing else: a
// resolved credential is never sent anywhere but its own host or this machine.
const OverrideVariable = "PUTNAMI_GITHUB_API_URL"

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(?::[0-9]{1,5})?$`)

// ValidHost reports whether host is a bare host name, optionally with a port:
// no scheme, path, query or user information.
func ValidHost(host string) bool {
	return hostPattern.MatchString(host)
}

// Endpoint is where one host's API answers.
type Endpoint struct {
	// Host is the GitHub host the credential belongs to.
	Host string
	// REST is the REST API root, without a trailing slash.
	REST *url.URL
	// GraphQL is the GraphQL endpoint.
	GraphQL *url.URL
}

// ResolveEndpoint returns the API roots of a host: api.github.com for
// github.com, and /api/v3 and /api/graphql on the host itself for GitHub
// Enterprise Server. getenv reads OverrideVariable.
func ResolveEndpoint(host string, getenv func(string) string) (Endpoint, error) {
	if host == "" {
		host = DefaultHost
	}
	if !ValidHost(host) {
		return Endpoint{}, fmt.Errorf("host %q is not a bare host name", host)
	}
	if override := getenv(OverrideVariable); override != "" {
		parsed, err := url.Parse(override)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
			parsed.RawQuery != "" || parsed.Fragment != "" || !loopback(parsed.Hostname()) {
			return Endpoint{}, fmt.Errorf("%s must be an http or https URL on the loopback interface, without credentials or query", OverrideVariable)
		}
		root := strings.TrimSuffix(parsed.String(), "/")
		return endpoint(host, root, root+"/graphql")
	}
	if strings.EqualFold(host, DefaultHost) {
		return endpoint(DefaultHost, "https://api.github.com", "https://api.github.com/graphql")
	}
	return endpoint(host, "https://"+host+"/api/v3", "https://"+host+"/api/graphql")
}

func endpoint(host, rest, graphQL string) (Endpoint, error) {
	restURL, err := url.Parse(rest)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse the REST root: %w", err)
	}
	graphQLURL, err := url.Parse(graphQL)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse the GraphQL endpoint: %w", err)
	}
	return Endpoint{Host: host, REST: restURL, GraphQL: graphQLURL}, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Path builds a REST path from segments, escaping each one, so a branch name
// or a label never changes the path's structure.
func Path(segments ...string) string {
	var builder strings.Builder
	for _, segment := range segments {
		builder.WriteByte('/')
		builder.WriteString(url.PathEscape(segment))
	}
	return builder.String()
}
