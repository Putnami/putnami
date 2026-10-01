// Command putnami-local-collaboration is the runtime of the
// @putnami/local-collaboration extension: the reference provider of the tasks
// and proposals collaboration contracts, backed by a store directory inside
// the workspace.
//
// Usage:
//
//	putnami-local-collaboration __putnami runtime-info   (the runtime handshake)
//	putnami-local-collaboration provider-tool            (one routed ToolCallRequest on stdin, one ToolCallResult on stdout)
//
// It is never called directly: the workspace binds it in options.collaboration
// and Putnami routes `putnami tasks|proposals <operation>` and the MCP tools
// `tasks.*` and `proposals.*` to it.
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

// providerToolArg routes the executable to the collaboration bridge.
const providerToolArg = "provider-tool"

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
	case slices.Equal(args, []string{providerToolArg}):
		if err := collab.Serve(context.Background(), stdin, stdout, newHandlers()); err != nil {
			fmt.Fprintf(stderr, "putnami-local-collaboration: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, "putnami-local-collaboration: unknown invocation; this runtime answers "+providerToolArg+" only through a workspace binding")
	return 2
}

// newHandlers is the dispatch table of the provider bridge.
func newHandlers() map[collab.OperationKey]collab.Handler {
	return provider.New().Handlers()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
