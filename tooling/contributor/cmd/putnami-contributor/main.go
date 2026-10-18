// Command putnami-contributor is the runtime of the @putnami/contributor
// extension.
//
// Usage:
//
//	putnami-contributor __putnami runtime-info    (the runtime handshake)
//	putnami-contributor language <mode> [flags]   (putnami change language)
//
// It answers the runtime handshake, serves the change commands that moved
// here, and refuses every other invocation. putnami launches a change command
// as an interactive subcommand and appends --putnamiContext <file>, which the
// command ignores.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"go.putnami.dev/sdk/extension/runtimeinfo"

	"go.putnami.dev/tooling/contributor/internal/host"
	"go.putnami.dev/tooling/contributor/internal/language"
)

// extensionName must equal the manifest's name: core compares the runtime
// handshake against the manifest that declared the runtime.
const extensionName = "@putnami/contributor"

// runtimeVersion is stamped at link time through
// options["@putnami/go"].version-var; an unstamped build reports it empty.
var runtimeVersion string

// contextFlag is the flag putnami appends to every launch, followed by the
// path of the job context file.
const contextFlag = "--putnamiContext"

// run is main with its process dependencies passed in. It returns the exit
// code.
func run(args []string, stdout, stderr io.Writer) int {
	return runIn(args, os.Stdin, stdout, stderr)
}

// runIn is run with the standard input passed in.
func runIn(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	handled, err := runtimeinfo.Handle(args, stdout, extensionName, runtimeVersion)
	if err != nil {
		fmt.Fprintf(stderr, "putnami-contributor: %v\n", err)
		return 1
	}
	if handled {
		return 0
	}
	if args = withoutContext(args); len(args) > 0 && args[0] == "language" {
		return runLanguage(args[1:], stdin, stdout, stderr)
	}
	fmt.Fprintln(stderr, "putnami-contributor: unknown invocation; this runtime answers the Putnami runtime handshake and the language command")
	return 2
}

// withoutContext returns args without the last --putnamiContext <file> pair.
func withoutContext(args []string) []string {
	for i := len(args) - 2; i >= 0; i-- {
		if args[i] == contextFlag {
			return append(append([]string{}, args[:i]...), args[i+2:]...)
		}
	}
	return args
}

// runLanguage runs putnami change language in the workspace root, which is
// the working directory putnami launches the command in.
func runLanguage(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "change language needs text, files, commits or tasks")
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "change language: %v\n", err)
		return 2
	}
	caller := host.NewCaller()
	defer func() { _ = caller.Close() }()
	runner := language.LanguageRunner{Root: root, Stdin: stdin, Out: stdout, Err: stderr, Call: caller.Call}
	return runner.RunLanguage(context.Background(), args[0], args[1:])
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
