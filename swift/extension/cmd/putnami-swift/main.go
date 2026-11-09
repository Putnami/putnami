// Command putnami-swift is the runtime of @putnami/swift: the stack core run
// with the Swift profile, the Swift detectors and the Swift runtime commands.
package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/docslinks"
	swift "go.putnami.dev/swift/extension"
	"go.putnami.dev/tooling/stacks"
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
	stack, err := swift.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs[stacks.TaskTest] = swift.TestJob(stack)
			jobs["lint-docs"] = docslinks.Job("Package.swift")
			jobs[swift.WorkspaceInstallJobName] = swift.WorkspaceInstallJob(stack)
			jobs[swift.ServeJobName] = swift.ServeJob(stack)
			jobs[swift.RunJobName] = swift.RunJob(stack)
			jobs[swift.DepsUpgradeJobName] = swift.DepsUpgradeJob(stack)
			jobs[swift.PackageDockerJobName] = swift.PackageDockerJob(stack)
			jobs[swift.PublishDockerJobName] = swift.PublishDockerJob()
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", swift.ExtensionName, err)
		return 1
	}
	return 0
}
