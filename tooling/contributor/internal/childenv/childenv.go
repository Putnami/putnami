// Package childenv builds the environment of a process the executable
// starts. No such process receives the host RPC channel or its token.
package childenv

import (
	"strings"

	wire "go.putnami.dev/protocol/extension/hostrpc"
)

// Repository removes the selectors that could redirect a Git command away
// from its explicit working directory, and the host channel. Transport
// credentials and tracing (including GIT_SSH_COMMAND and GIT_ASKPASS) pass
// through unchanged. It matches the core's helper of the same purpose.
func Repository(environ []string) []string {
	cleaned := make([]string, 0, len(environ))
	for _, entry := range WithoutHostChannel(environ) {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_CONFIG") {
			continue
		}
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX", "GIT_CEILING_DIRECTORIES",
			"GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_NAMESPACE":
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}

// WithoutHostChannel removes the host RPC channel and its token, which no
// process the executable starts has a use for.
func WithoutHostChannel(environ []string) []string {
	cleaned := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if name == wire.SocketEnv || name == wire.TokenEnv {
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}
