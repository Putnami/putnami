// Command putnami-contributor is the runtime of the @putnami/contributor
// extension.
//
// Usage:
//
//	putnami-contributor __putnami runtime-info    (the runtime handshake)
//	putnami-contributor language <mode> [flags]   (putnami change language)
//	putnami-contributor verify [flags]            (putnami change verify)
//	putnami-contributor finalize [flags]          (putnami change finalize)
//
// It answers the runtime handshake, serves the change commands that moved
// here, and refuses every other invocation. putnami launches a change command
// as an interactive subcommand and appends --putnamiContext <file>. finalize
// reads the output mode and the project selection from that job context; the
// other commands ignore it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	sdkcontext "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/runtimeinfo"

	"go.putnami.dev/tooling/contributor/internal/finalize"
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

// languageRemedy and finalizeRemedy name the launch that opens the host
// channel, for a run that needs the channel and has none.
const (
	languageRemedy = "the tasks mode needs an interactive launch by putnami, which opens the host channel it reads the tasks provider through; run `putnami change language tasks`"
	finalizeRemedy = "this command needs an interactive launch by putnami, which opens the host channel it reaches the collaboration providers and the workspace through; run `putnami change finalize`"
)

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
	contextFile := contextPath(args)
	args = withoutContext(args)
	if len(args) > 0 && args[0] == "language" {
		return runLanguage(args[1:], contextFile, stdin, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "verify" {
		return runVerify(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "finalize" {
		return runFinalize(args[1:], contextFile, stdout, stderr)
	}
	fmt.Fprintln(stderr, "putnami-contributor: unknown invocation; this runtime answers the Putnami runtime handshake and the language, verify and finalize commands")
	return 2
}

// contextIndex returns the index of the last --putnamiContext <file> pair,
// or -1 when args has none.
func contextIndex(args []string) int {
	for i := len(args) - 2; i >= 0; i-- {
		if args[i] == contextFlag {
			return i
		}
	}
	return -1
}

// contextPath returns the file of the last --putnamiContext <file> pair, or
// "" when args has none.
func contextPath(args []string) string {
	if i := contextIndex(args); i >= 0 {
		return args[i+1]
	}
	return ""
}

// withoutContext returns args without the last --putnamiContext <file> pair.
func withoutContext(args []string) []string {
	if i := contextIndex(args); i >= 0 {
		return append(append([]string{}, args[:i]...), args[i+2:]...)
	}
	return args
}

// runLanguage runs putnami change language in the workspace root, which is
// the working directory putnami launches the command in. A file argument of the
// text mode resolves against the directory the user typed the command in,
// which the job context records as invocationDir.
func runLanguage(args []string, contextFile string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "change language needs text, files, commits or tasks")
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "change language: %v\n", err)
		return 2
	}
	caller := host.Take(languageRemedy)
	defer func() { _ = caller.Close() }()
	runner := language.LanguageRunner{Root: root, Stdin: stdin, Out: stdout, Err: stderr, Call: caller.Call,
		ResolvePath: invocationResolver(contextFile)}
	return runner.RunLanguage(context.Background(), args[0], args[1:])
}

// invocationResolver returns the function that resolves a path argument as the
// user typed it. A launch without a readable context file, such as a run of
// the executable by hand, resolves against the working directory: the
// command's checks do not depend on the context, so an unreadable one is not
// an error here.
func invocationResolver(contextFile string) func(string) string {
	if contextFile == "" {
		return nil
	}
	launch, err := sdkcontext.Parse(contextFile)
	if err != nil {
		return nil
	}
	return func(p string) string { return sdkcontext.ResolvePath(launch, p) }
}

// runVerify runs putnami change verify in its working directory: putnami
// launches it in the workspace root, or in the caller's directory outside a
// workspace. Its stdout is one verdict document; a not-verified verdict exits
// 1 with nothing on stderr. It takes the host channel out of the environment
// before it starts any process.
func runVerify(args []string, stdout, stderr io.Writer) int {
	// verify only reads reports, and a report read without a channel is a
	// verify.NoChannelError with its own message, so it needs no remedy.
	reports := host.Take("")
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

// runFinalize runs putnami change finalize in the workspace root, which is
// the working directory putnami launches the command in. It takes the host
// channel out of the environment before it starts any process, and shares
// one connection among every call into the host. SIGINT and SIGTERM cancel
// the run.
func runFinalize(args []string, contextFile string, stdout, stderr io.Writer) int {
	channel := host.Take(finalizeRemedy)
	defer func() { _ = channel.Close() }()
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "change finalize: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	command := finalize.Command{Root: root, ContextFile: contextFile, Host: channel, Stdout: stdout, Stderr: stderr}
	return command.Run(ctx, args)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
