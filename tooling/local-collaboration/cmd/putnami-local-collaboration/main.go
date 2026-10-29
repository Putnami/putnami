// Command putnami-local-collaboration is the runtime of the
// @putnami/local-collaboration extension: the reference provider of the tasks,
// proposals and missions collaboration contracts, backed by a store directory
// inside the workspace.
//
// Usage:
//
//	putnami-local-collaboration __putnami runtime-info   (the runtime handshake)
//	putnami-local-collaboration provider                 (one routed ProviderRequest on stdin, one Response on stdout)
//
// It is never called directly: the workspace binds it in options.collaboration
// and Putnami routes `putnami tasks|proposals|missions <operation>` and the
// host call collaboration.call to it; missions.sync is host-only.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"

	protocolcli "go.putnami.dev/protocol/cli"
	collab "go.putnami.dev/protocol/collaboration"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/local-collaboration/internal/provider"
)

// extensionName must equal the manifest's name: core compares the runtime
// handshake against the manifest that declared the runtime.
const extensionName = "@putnami/local-collaboration"

// providerArg routes the executable to the collaboration provider.
const providerArg = "provider"

// runtimeVersion is stamped at link time through
// options["@putnami/go"].version-var; an unstamped build reports it empty.
var runtimeVersion string

// run is main with its process dependencies passed in. It returns the exit
// code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch {
	case slices.Equal(args, []string{"__putnami", "runtime-info"}):
		if err := json.NewEncoder(stdout).Encode(runtimeproto.Info{
			Extension:       extensionName,
			Version:         runtimeVersion,
			Platform:        runtime.GOOS + "/" + runtime.GOARCH,
			CLIContract:     protocolcli.CurrentContract,
			RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
			RuntimeABI:      runtimeproto.RuntimeABIVersion,
		}); err != nil {
			fmt.Fprintf(stderr, "putnami-local-collaboration: %v\n", err)
			return 1
		}
		return 0
	case slices.Equal(args, []string{providerArg}):
		if err := collab.Serve(context.Background(), stdin, stdout, newHandlers()); err != nil {
			fmt.Fprintf(stderr, "putnami-local-collaboration: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, "putnami-local-collaboration: unknown invocation; this runtime answers "+providerArg+" only through a workspace binding")
	return 2
}

// newHandlers is the dispatch table of the provider bridge.
func newHandlers() map[collab.OperationKey]collab.Handler {
	return provider.New().Handlers()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
