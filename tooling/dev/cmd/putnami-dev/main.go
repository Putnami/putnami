// Command putnami-dev is the runtime of the @putnami/dev extension.
//
// Usage:
//
//	putnami-dev __putnami runtime-info          (the runtime handshake)
//	putnami-dev version get [flags]             (putnami version get)
//	putnami-dev version tag [flags]             (putnami version tag)
//	putnami-dev compose up <project> [flags]    (putnami compose <project>)
//	putnami-dev qualify run <project> [flags]   (putnami qualify <project>)
//
// It answers the runtime handshake, serves the version, compose and qualify
// commands, and refuses every other invocation. putnami launches each command
// as an interactive subcommand in the workspace root and appends
// --putnamiContext <file>; the command reads the output mode, --dry-run and
// the workspace membership from that job context. compose and qualify start
// each member of a composition as a serve run of the putnami executable that
// launched them, which putnami names in PUTNAMI_CLI_EXECUTABLE. The
// development verbs the manifest declares run in the language extensions,
// never here.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/resultv2"
	"go.putnami.dev/sdk/extension/runtimeinfo"

	"go.putnami.dev/tooling/dev/internal/compose"
	"go.putnami.dev/tooling/dev/internal/host"
	"go.putnami.dev/tooling/dev/internal/qualify"
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
	switch {
	case len(args) >= 2 && args[0] == "version" && (args[1] == "get" || args[1] == "tag"):
		return runVersion(args[1], args[2:], contextFile, stdout, stderr)
	case len(args) >= 2 && args[0] == "compose" && args[1] == "up":
		return runWorkload(compose.CommandPath, args[2:], contextFile, stdout, stderr, func(launch workloadLaunch) int {
			return compose.Run(compose.Request{
				Ctx: context.Background(), Workspace: launch.workspace, Args: launch.args, Output: launch.output,
				Executable: launch.executable, Stdout: stdout, Stderr: stderr,
			})
		})
	case len(args) >= 2 && args[0] == "qualify" && args[1] == "run":
		return runWorkload(qualify.CommandPath, args[2:], contextFile, stdout, stderr, func(launch workloadLaunch) int {
			return qualify.Run(qualify.Request{
				Ctx: context.Background(), Workspace: launch.workspace, Args: launch.args, Output: launch.output,
				Executable: launch.executable, Stdout: stdout, Stderr: stderr,
			})
		})
	}
	fmt.Fprintln(stderr, "putnami-dev: unknown invocation; this runtime answers the Putnami runtime handshake and the version get and tag, compose up and qualify run commands")
	return 2
}

// cliExecutableEnv names the putnami executable that launched an interactive
// command. A composition starts its members' serve runs with it.
const cliExecutableEnv = "PUTNAMI_CLI_EXECUTABLE"

// workloadLaunch is what an interactive compose or qualify launch reads from
// putnami before the command runs.
type workloadLaunch struct {
	// args are the user's arguments after the subcommand.
	args []string
	// output is the output mode putnami resolved.
	output protocolcli.OutputMode
	// workspace is the workspace membership the job context carries.
	workspace *compose.Workspace
	// executable is the absolute path of the putnami executable.
	executable string
}

// runWorkload reads a compose or qualify launch, then runs the command. It
// takes the host channel out of the environment first, so no serve run the
// command starts inherits it. A launch that names no job context, no
// workspace membership or no absolute putnami executable is refused before
// anything starts.
func runWorkload(command string, args []string, contextFile string, stdout, stderr io.Writer, run func(workloadLaunch) int) int {
	_ = host.Take("").Close()
	if contextFile == "" {
		fmt.Fprintf(stderr, "putnami: %s needs the job context of an interactive launch by putnami; run `putnami %s <project>`\n", command, command)
		return protocolcli.ExitFailure
	}
	launchContext, err := job.ParseFile(contextFile)
	if err != nil {
		fmt.Fprintf(stderr, "putnami: %s: %v\n", command, err) //nolint:gosec // G705: stderr is a terminal or a log, never an HTTP response
		return protocolcli.ExitFailure
	}
	launch := workloadLaunch{args: args, output: protocolcli.OutputMode(launchContext.Params.String("output"))}
	launch.workspace, launch.executable, err = readWorkloadLaunch(command, launchContext)
	if err != nil {
		return resultv2.Emit(stdout, stderr, launch.output, command, "", nil, err)
	}
	return run(launch)
}

// readWorkloadLaunch returns the workspace membership and the putnami
// executable of a launch.
func readWorkloadLaunch(command string, launchContext *job.Context) (*compose.Workspace, string, error) {
	if len(launchContext.WorkspaceProjects) == 0 {
		return nil, "", fmt.Errorf("%s: the job context carries no workspace membership; run `putnami %s <project>` in a workspace", command, command)
	}
	ws, err := compose.NewWorkspace(launchContext.WorkspaceRoot, wsproto.Load(launchContext.WorkspaceRoot), launchContext.WorkspaceProjects)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", command, err)
	}
	executable := os.Getenv(cliExecutableEnv)
	if executable == "" || !filepath.IsAbs(executable) {
		return nil, "", errors.New(command + ": " + cliExecutableEnv + " names no absolute putnami executable to start the serve runs with; run `putnami " + command + " <project>`")
	}
	return ws, executable, nil
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
