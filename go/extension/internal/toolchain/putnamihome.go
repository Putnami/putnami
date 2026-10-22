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
// the CLI provisions, one go-<version> directory each, shared by every
// workspace of the machine: toolchains/go under the Putnami home.
//
// When the environment names no home, the Putnami home is .putnami under
// workspaceRoot. The CLI resolves the home the same way when it provisions a
// toolchain, so the release the provisioned candidate names is
// go-<version>/go/bin/go here.
func ResolveGoToolchainRoot(lookup func(string) string, workspaceRoot string) string {
	return putnamihome.ToolchainRoot(lookup, workspaceRoot, goToolchainName)
}
