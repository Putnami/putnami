// Command clientgen emits a Go REST client from a provider project's generated
// OpenAPI spec, with no running app.
//
// It is the standalone Go counterpart of the in-app api.Clients describer: the
// workspace `clientgen` command runs it (in its own Go toolchain) to produce a Go
// client for a provider written in ANOTHER language — e.g. a TypeScript service —
// by reading that provider's OWN .gen spec. Cross-language client emission is thus
// mediated through the shared spec artifact and never shells one extension out to
// another.
//
// Usage:
//
//	clientgen [--project DIR]
//
// --project defaults to the current directory (the workspace runner sets the task
// cwd to the project root). The project must already have been built (putnami
// build) so its .gen/clientgen/config.json and OpenAPI spec exist.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"go.putnami.dev/api"
)

func main() {
	project := flag.String("project", "", "provider project root (defaults to the current directory)")
	// The workspace task runner always appends --putnamiContext; accept it so flag
	// parsing does not fail. The task sets cwd to the project root, so the default
	// "." resolution below is authoritative and the context path is not needed.
	_ = flag.String("putnamiContext", "", "path to the job context JSON (provided by the workspace runner; unused)")
	flag.Parse()

	root := *project
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clientgen:", err)
		os.Exit(1)
	}

	res, err := api.GenerateProjectClients(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clientgen:", err)
		os.Exit(1)
	}
	for _, operationID := range res.Omitted {
		fmt.Fprintf(os.Stderr, "clientgen: left operation %s out of the Go client (go.omitOperations)\n", operationID)
	}
	if res.Generated {
		fmt.Printf("clientgen: wrote %d file(s) to %s\n", len(res.Files), res.OutputDir)
		return
	}
	fmt.Println("clientgen: no Go client emitted (no go target configured, or the spec exposes no API surface)")
}
