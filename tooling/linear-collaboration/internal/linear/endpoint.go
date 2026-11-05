// Package linear is the Linear GraphQL client of the Linear collaboration
// provider. It reads the API key from LINEAR_API_KEY, sends every request
// exactly once unless it is a read, and classifies every failure by what it
// proves about the remote state: a request that never left this process, one
// whose outcome is unknown, and a definite answer.
package linear

import (
	"net/url"

	"go.putnami.dev/sdk/extension/collabhelper"
)

// DefaultEndpoint is Linear's GraphQL endpoint.
const DefaultEndpoint = "https://api.linear.app/graphql"

// OverrideVariable names the environment variable that points the client at
// another GraphQL endpoint. Only an http or https URL on the loopback interface
// is accepted, so the variable can reach a local stand-in and nothing else: the
// API key is never sent anywhere but Linear or this machine.
const OverrideVariable = "PUTNAMI_LINEAR_API_URL"

// ResolveEndpoint returns the GraphQL endpoint: DefaultEndpoint, or the
// loopback URL OverrideVariable names. getenv reads the variable.
func ResolveEndpoint(getenv func(string) string) (*url.URL, error) {
	override := getenv(OverrideVariable)
	if override == "" {
		return url.Parse(DefaultEndpoint)
	}
	return collabhelper.LoopbackOverride(OverrideVariable, override)
}
