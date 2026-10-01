package toolchain

import (
	"go.putnami.dev/sdk/extension/putnamihome"
)

// goToolchainName names the Go toolchain in the toolchains subtree of a
// Putnami home.
const goToolchainName = "go"

// ResolvePutnamiHome returns the Putnami home the environment names, or ""
// when it names none: PUTNAMI_HOME, else .putnami under the user's home
// directory. lookup reads one variable.
func ResolvePutnamiHome(lookup func(string) string) string {
	return putnamihome.Resolve(lookup)
}

// ResolveGoToolchainRoot returns the directory that holds the Go releases
// `putnami install` installs, one go-<version> directory each, shared by every
// workspace of the machine: toolchains/go under the Putnami home.
//
// When the environment names no home, the Putnami home is .putnami under
// workspaceRoot. The CLI resolves the home the same way for the putnami-home
// candidates of a runtime toolchain, so a release installed here is the one
// the candidate toolchains/go/go-{version}/go/bin/go names.
func ResolveGoToolchainRoot(lookup func(string) string, workspaceRoot string) string {
	return putnamihome.ToolchainRoot(lookup, workspaceRoot, goToolchainName)
}
