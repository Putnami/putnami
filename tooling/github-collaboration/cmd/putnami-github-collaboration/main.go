// Command putnami-github-collaboration is the runtime of the
// @putnami/github-collaboration extension: the GitHub provider of the tasks
// and proposals collaboration contracts, over the issues and pull requests of
// one repository.
//
// Usage:
//
//	putnami-github-collaboration __putnami runtime-info   (the runtime handshake)
//	putnami-github-collaboration provider                 (one routed ProviderRequest on stdin, one Response on stdout)
//
// It is never called directly: the workspace binds it in options.collaboration
// and Putnami routes `putnami tasks|proposals <operation>` and the host call
// collaboration.call to it.
package main

import (
	"bytes"
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
	"go.putnami.dev/tooling/github-collaboration/internal/provider"
)

// extensionName must equal the manifest's name: core compares the runtime
// handshake against the manifest that declared the runtime.
const extensionName = "@putnami/github-collaboration"

// providerArg routes the executable to the collaboration provider.
const providerArg = "provider"

// runtimeVersion is stamped at link time through
// options["@putnami/go"].version-var; an unstamped build reports it empty.
var runtimeVersion string

// run is main with its process dependencies passed in. It returns the exit
// code. The provider answer is written whole, after every credential the
// provider resolved has been redacted from it.
func run(args []string, p *provider.Provider, stdin io.Reader, stdout, stderr io.Writer) int {
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
			fmt.Fprintf(stderr, "putnami-github-collaboration: %v\n", err)
			return 1
		}
		return 0
	case slices.Equal(args, []string{providerArg}):
		p.Client.UserAgent = "putnami-github-collaboration/" + runtimeVersion
		var answer bytes.Buffer
		if err := collab.Serve(context.Background(), stdin, &answer, p.Handlers()); err != nil {
			fmt.Fprintln(stderr, "putnami-github-collaboration: the request could not be read")
			return 1
		}
		if _, err := stdout.Write(p.Redact(answer.Bytes())); err != nil {
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, "putnami-github-collaboration: unknown invocation; this runtime answers "+providerArg+" only through a workspace binding")
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], provider.New(), os.Stdin, os.Stdout, os.Stderr))
}
