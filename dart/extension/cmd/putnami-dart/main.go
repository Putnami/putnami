// Command putnami-dart is the runtime of @putnami/dart: the stack core run
// with the Dart profile and the Dart detector, and the Dart runtime commands.
package main

import (
	"fmt"
	"io"
	"os"

	dart "go.putnami.dev/dart/extension"
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
	stack, err := dart.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs["lint-docs"] = docslinks.Job("pubspec.yaml")
			jobs[dart.InstallJobName] = dart.InstallJob(stack)
			jobs[dart.WorkspaceFetchJobName] = dart.WorkspaceFetchJob(stack)
			jobs[dart.WorkspaceInstallJobName] = dart.WorkspaceInstallJob(stack)
			jobs[dart.DepsUpgradeJobName] = dart.DepsUpgradeJob(stack)
			jobs[dart.ServeJobName] = dart.ServeJob(stack)
			jobs[dart.RunJobName] = dart.RunJob(stack)
			jobs[dart.PackageDockerJobName] = dart.PackageDockerJob(stack)
			jobs[dart.PublishDockerJobName] = dart.PublishDockerJob()
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", dart.ExtensionName, err)
		return 1
	}
	return 0
}
