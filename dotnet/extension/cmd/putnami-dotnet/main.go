// Command putnami-dotnet is the runtime of @putnami/dotnet: the stack core run
// with the .NET profile and the .NET detector.
package main

import (
	"fmt"
	"io"
	"os"

	dotnet "go.putnami.dev/dotnet/extension"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/docslinks"
)

var runtimeVersion string

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, func(jobs map[string]cli.JobFunc) {
		cli.RunSubcommand(jobs, cli.WithSkipEmptyProject(false))
	}))
}

// run dispatches one invocation and returns the exit code. A control call's
// stdout carries exactly one document, so errors go to stderr.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, runJobs func(map[string]cli.JobFunc)) int {
	stack, err := dotnet.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs["lint-docs"] = docslinks.Job()
			jobs[dotnet.WorkspaceInstallJobName] = dotnet.WorkspaceInstallJob(stack)
			jobs[dotnet.WorkspaceFetchJobName] = dotnet.WorkspaceFetchJob(stack)
			jobs[dotnet.DepsUpgradeJobName] = dotnet.DepsUpgradeJob(stack)
			jobs[dotnet.ServeJobName] = dotnet.ServeJob(stack)
			jobs[dotnet.RunJobName] = dotnet.RunJob(stack)
			jobs[dotnet.PackageDockerJobName] = dotnet.PackageDockerJob(stack)
			jobs[dotnet.PublishDockerJobName] = dotnet.PublishDockerJob()
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", dotnet.ExtensionName, err)
		return 1
	}
	return 0
}
