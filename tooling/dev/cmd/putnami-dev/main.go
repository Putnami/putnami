// Command putnami-dev is the runtime of the @putnami/dev extension.
//
// Usage:
//
//	putnami-dev __putnami runtime-info    (the runtime handshake)
//	putnami-dev version get [flags]       (putnami version get)
//	putnami-dev version tag [flags]       (putnami version tag)
//
// It answers the runtime handshake, serves the version commands, and refuses
// every other invocation. putnami launches a version command as an
// interactive subcommand in the workspace root and appends
// --putnamiContext <file>; the command reads the output mode and --dry-run
// from that job context. The development verbs the manifest declares run in
// the language extensions, never here.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/runtimeinfo"

	"go.putnami.dev/tooling/dev/internal/host"
	"go.putnami.dev/tooling/dev/internal/version"
)

// extensionName must equal the manifest's name: core compares the runtime
// handshake against the manifest that declared the runtime.
const extensionName = "@putnami/dev"

// runtimeVersion is stamped at link time through
// options["@putnami/go"].version-var; an unstamped build reports it empty.
var runtimeVersion string

// contextFlag is the flag putnami appends to every launch, followed by the
// path of the job context file.
const contextFlag = "--putnamiContext"

// versionRemedy names the launch that opens the host channel, for a version
// command that runs without one.
const versionRemedy = "this command needs an interactive launch by putnami, which opens the host channel it reads the version lines through; run `putnami version %s`"

// run is main with its process dependencies passed in. It returns the exit
// code.
func run(args []string, stdout, stderr io.Writer) int {
	handled, err := runtimeinfo.Handle(args, stdout, extensionName, runtimeVersion)
	if err != nil {
		fmt.Fprintf(stderr, "putnami-dev: %v\n", err)
		return 1
	}
	if handled {
		return 0
	}
	contextFile := contextPath(args)
	args = withoutContext(args)
	if len(args) >= 2 && args[0] == "version" && (args[1] == "get" || args[1] == "tag") {
		return runVersion(args[1], args[2:], contextFile, stdout, stderr)
	}
	fmt.Fprintln(stderr, "putnami-dev: unknown invocation; this runtime answers the Putnami runtime handshake and the version get and tag commands")
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

// runVersion runs `putnami version <sub>` in the workspace root, which is the
// working directory putnami launches the command in. It takes the host
// channel out of the environment before it starts any process. SIGINT and
// SIGTERM cancel the run.
func runVersion(sub string, args []string, contextFile string, stdout, stderr io.Writer) int {
	channel := host.Take(fmt.Sprintf(versionRemedy, sub))
	defer func() { _ = channel.Close() }()
	command := version.Command{Host: channel, Stdout: stdout, Stderr: stderr}
	if contextFile != "" {
		launch, err := job.ParseFile(contextFile)
		if err != nil {
			fmt.Fprintf(stderr, "version %s: %v\n", sub, err) //nolint:gosec // G705: stderr is a terminal or a log, never an HTTP response
			return protocolcli.ExitFailure
		}
		command.Output = protocolcli.OutputMode(launch.Params.String("output"))
		command.DryRun = launch.Params.Bool("dry-run", false)
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "version %s: %v\n", sub, err) //nolint:gosec // G705: stderr is a terminal or a log, never an HTTP response
		return protocolcli.ExitFailure
	}
	command.Root = root
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if sub == "get" {
		return command.Get(ctx, args)
	}
	return command.Tag(ctx, args)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
