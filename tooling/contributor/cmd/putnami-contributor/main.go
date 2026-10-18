// Command putnami-contributor is the runtime of the @putnami/contributor
// extension.
//
// Usage:
//
//	putnami-contributor __putnami runtime-info    (the runtime handshake)
//	putnami-contributor language <mode> [flags]   (putnami change language)
//	putnami-contributor verify [flags]            (putnami change verify)
//
// It answers the runtime handshake, serves the change commands that moved
// here, and refuses every other invocation. putnami launches a change command
// as an interactive subcommand and appends --putnamiContext <file>, which the
// command ignores.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"go.putnami.dev/sdk/extension/runtimeinfo"

	"go.putnami.dev/tooling/contributor/internal/host"
	"go.putnami.dev/tooling/contributor/internal/language"
	"go.putnami.dev/tooling/contributor/internal/verify"
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
	args = withoutContext(args)
	if len(args) > 0 && args[0] == "language" {
		return runLanguage(args[1:], stdin, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "verify" {
		return runVerify(args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, "putnami-contributor: unknown invocation; this runtime answers the Putnami runtime handshake and the language and verify commands")
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

// runVerify runs putnami change verify in its working directory: putnami
// launches it in the workspace root, or in the caller's directory outside a
// workspace. Its stdout is one verdict document; a not-verified verdict exits
// 1 with nothing on stderr. It takes the host channel out of the environment
// before it starts any process.
func runVerify(args []string, stdout, stderr io.Writer) int {
	reports := host.TakeReports()
	defer func() { _ = reports.Close() }()
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "change verify: resolve the working directory: %v\n", err)
		return 1
	}
	err = verify.Verifier{Dir: dir, Stdout: stdout, RecoverReport: reports.Recover}.Run(args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, verify.ErrNotVerified):
		return 1
	}
	fmt.Fprintf(stderr, "change verify: %v\n", err)
	return 1
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
