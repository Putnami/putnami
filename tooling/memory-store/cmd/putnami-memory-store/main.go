// Command putnami-memory-store is the runtime of the @putnami/memory-store
// extension: a provider of the memory collaboration contract that keeps
// records in a directory (backend "file") or on a Git branch (backend "git"),
// as the workspace binding's settings choose.
//
// Usage:
//
//	putnami-memory-store __putnami runtime-info   (the runtime handshake)
//	putnami-memory-store provider-tool            (one routed ToolCallRequest on stdin, one ToolCallResult on stdout)
//
// It is never called directly: the workspace binds it in
// options.collaboration.memory and Putnami routes `putnami memory <operation>`
// and the MCP tools `memory.*` to it.
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
	"go.putnami.dev/tooling/memory-store/internal/memory"
)

// extensionName must equal the manifest's name: core compares the runtime
// handshake against the manifest that declared the runtime.
const extensionName = "@putnami/memory-store"

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
			fmt.Fprintf(stderr, "putnami-memory-store: %v\n", err)
			return 1
		}
		return 0
	case slices.Equal(args, []string{providerToolArg}):
		if err := collab.Serve(context.Background(), stdin, stdout, newHandlers()); err != nil {
			fmt.Fprintf(stderr, "putnami-memory-store: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, "putnami-memory-store: unknown invocation; this runtime answers "+providerToolArg+" only through a workspace binding")
	return 2
}

// newHandlers is the dispatch table of the provider bridge.
func newHandlers() map[collab.OperationKey]collab.Handler {
	return memory.New().Handlers()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
