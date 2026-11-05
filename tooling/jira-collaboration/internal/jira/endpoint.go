// Package jira is the Jira Cloud REST client of the Jira collaboration
// provider. It reads its credential from the environment, sends every request
// exactly once unless it is a read, and classifies every failure by what it
// proves about the remote state: a request that never left this process, one
// whose outcome is unknown, and a definite answer.
package jira

import (
	"fmt"
	"net/url"
	"strings"

	"go.putnami.dev/sdk/extension/collabhelper"
)

// OverrideVariable names the environment variable that points the client at
// another API root. Only an http or https URL on the loopback interface is
// accepted, so the variable can reach a local stand-in and nothing else: the
// credential is never sent anywhere but the bound site or this machine.
const OverrideVariable = "PUTNAMI_JIRA_API_URL"

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
	if !collabhelper.ValidHost(site) {
		return Endpoint{}, fmt.Errorf("site %q is not a bare host name", site)
	}
	root := "https://" + site
	if override := getenv(OverrideVariable); override != "" {
		parsed, err := collabhelper.LoopbackOverride(OverrideVariable, override)
		if err != nil {
			return Endpoint{}, err
		}
		root = strings.TrimSuffix(parsed.String(), "/")
	}
	rest, err := url.Parse(root + "/rest/api/2")
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse the REST root: %w", err)
	}
	return Endpoint{Site: site, REST: rest}, nil
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
