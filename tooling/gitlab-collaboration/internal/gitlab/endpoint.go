// Package gitlab is the GitLab REST API v4 client of the GitLab
// collaboration provider. It resolves credentials the way the glab CLI finds
// them, sends every request exactly once unless it is a read, and classifies
// every failure by what it proves about the remote state: a request that
// never left this process, one whose outcome is unknown, and a definite
// answer.
package gitlab

import (
	"fmt"
	"net/url"
	"strings"

	"go.putnami.dev/sdk/extension/collabhelper"
)

// DefaultHost is the GitLab.com host.
const DefaultHost = "gitlab.com"

// OverrideVariable names the environment variable that points the client at
// another API root. Only an http or https URL on the loopback interface is
// accepted, so the variable can reach a local stand-in and nothing else: a
// resolved credential is never sent anywhere but its own instance or this
// machine.
const OverrideVariable = "PUTNAMI_GITLAB_API_URL"

// Endpoint is where one instance's API answers.
type Endpoint struct {
	// Host is the GitLab instance the credential belongs to.
	Host string
	// REST is the REST API v4 root, without a trailing slash.
	REST *url.URL
}

// ResolveEndpoint returns the REST API v4 root of an instance:
// https://<host>/api/v4, or the loopback root OverrideVariable names. getenv
// reads OverrideVariable.
func ResolveEndpoint(host string, getenv func(string) string) (Endpoint, error) {
	if host == "" {
		host = DefaultHost
	}
	if !collabhelper.ValidHost(host) {
		return Endpoint{}, fmt.Errorf("host %q is not a bare host name", host)
	}
	root := "https://" + host
	if override := getenv(OverrideVariable); override != "" {
		parsed, err := collabhelper.LoopbackOverride(OverrideVariable, override)
		if err != nil {
			return Endpoint{}, err
		}
		root = strings.TrimSuffix(parsed.String(), "/")
	}
	rest, err := url.Parse(root + "/api/v4")
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse the REST root: %w", err)
	}
	return Endpoint{Host: host, REST: rest}, nil
}

// Path builds a REST path from segments, escaping each one, so a project path
// becomes one segment (group%2Fname) and a username never changes the path's
// structure.
func Path(segments ...string) string {
	var builder strings.Builder
	for _, segment := range segments {
		builder.WriteByte('/')
		builder.WriteString(url.PathEscape(segment))
	}
	return builder.String()
}
