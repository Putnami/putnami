// Command clientgen emits a Go REST client from a provider's contract, with no
// running app. It installs as putnami-client-generate-go.
//
// Usage:
//
//	putnami-client-generate-go --contract <openapi.json> --output <dir> \
//	    [--package <go package>] [--client <type name>] [--omit <operationId>]...
//
// It reads the provider's committed contract and writes exactly
// <output>/client.gen.go: no manifest, no go.mod and no putnami.json. The
// client project's client-generate command runs it with the options of its
// generatedFrom. A contract without a document-level x-putnami-client fails.
// A contract that declares no operation removes <output>/client.gen.go. An
// --omit operation the contract does not declare fails. Errors go to stderr
// with a non-zero exit status.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/api"
)

func main() {
	// The api module imports go.putnami.dev/app, so the Go extension's describe
	// step runs this command with PUTNAMI_DESCRIBE set, as it runs every
	// application of the module. The command is no application and has nothing
	// to describe.
	if os.Getenv("PUTNAMI_DESCRIBE") != "" {
		return
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// omitList collects every --omit value in order.
type omitList []string

func (o *omitList) String() string { return strings.Join(*o, ",") }

func (o *omitList) Set(value string) error {
	*o = append(*o, value)
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("putnami-client-generate-go", flag.ContinueOnError)
	flags.SetOutput(stderr)
	contract := flags.String("contract", "", "path of the provider's openapi.json")
	output := flags.String("output", "", "directory that receives client.gen.go")
	packageName := flags.String("package", "client", "Go package name of the generated file")
	clientName := flags.String("client", "Client", "generated client type name")
	var omit omitList
	flags.Var(&omit, "omit", "operationId to leave out of the client (repeatable)")
	// The workspace task runner appends --putnamiContext to every task. The
	// flags above carry everything the emission reads, so it is accepted and
	// ignored.
	_ = flags.String("putnamiContext", "", "job context path the workspace runner passes (unused)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	if *contract == "" || *output == "" {
		return fail(stderr, fmt.Errorf("--contract and --output are required"))
	}
	contractPath, err := filepath.Abs(*contract)
	if err != nil {
		return fail(stderr, err)
	}
	outputDir, err := filepath.Abs(*output)
	if err != nil {
		return fail(stderr, err)
	}
	contractJSON, err := os.ReadFile(contractPath) //nolint:gosec // G304: the caller names the contract to read
	if err != nil {
		return fail(stderr, fmt.Errorf("read contract: %w", err))
	}
	result, err := api.EmitGoClient(contractJSON, outputDir, api.EmitGoClientOptions{
		PackageName:    *packageName,
		ClientName:     *clientName,
		OmitOperations: omit,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("%s: %w", contractPath, err))
	}
	for _, operationID := range result.Omitted {
		if err := say(stderr, "left operation %s out of the Go client", operationID); err != nil {
			return 1
		}
	}
	report := fmt.Sprintf("%s declares no operation; no client written", contractPath)
	if result.Written {
		report = "wrote " + filepath.Join(outputDir, api.GoClientFile)
	}
	if err := say(stdout, "%s", report); err != nil {
		return 1
	}
	return 0
}

// say writes one line prefixed with the command name.
func say(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, "putnami-client-generate-go: "+format+"\n", args...)
	return err
}

// fail reports err on stderr and returns the failure exit status. A failed
// write changes nothing: the status already says the command failed.
func fail(stderr io.Writer, err error) int {
	if writeErr := say(stderr, "%v", err); writeErr != nil {
		return 1
	}
	return 1
}
