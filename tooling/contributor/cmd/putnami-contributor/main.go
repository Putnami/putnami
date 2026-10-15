// Command putnami-contributor is the runtime of the @putnami/contributor
// extension.
//
// Usage:
//
//	putnami-contributor __putnami runtime-info   (the runtime handshake)
//
// It answers the runtime handshake and refuses every other invocation.
package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/sdk/extension/runtimeinfo"
)

// extensionName must equal the manifest's name: core compares the runtime
// handshake against the manifest that declared the runtime.
const extensionName = "@putnami/contributor"

// runtimeVersion is stamped at link time through
// options["@putnami/go"].version-var; an unstamped build reports it empty.
var runtimeVersion string

// run is main with its process dependencies passed in. It returns the exit
// code.
func run(args []string, stdout, stderr io.Writer) int {
	handled, err := runtimeinfo.Handle(args, stdout, extensionName, runtimeVersion)
	if err != nil {
		fmt.Fprintf(stderr, "putnami-contributor: %v\n", err)
		return 1
	}
	if handled {
		return 0
	}
	fmt.Fprintln(stderr, "putnami-contributor: unknown invocation; this runtime answers only the Putnami runtime handshake")
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
