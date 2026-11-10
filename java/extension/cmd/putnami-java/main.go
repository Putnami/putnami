// Command putnami-java is the runtime of @putnami/java: the stack core run
// with the Java profile and the Gradle and Maven detectors.
package main

import (
	"fmt"
	"io"
	"os"

	java "go.putnami.dev/java/extension"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/docslinks"
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
	stack, err := java.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs[stacks.TaskTest] = java.TestJob(stack)
			// A directory that holds a build file is a component or a build
			// root of its own, which answers for its own documents.
			jobs["lint-docs"] = docslinks.Job(stack.Profile.Markers...)
			jobs[java.ServeJobName] = java.ServeJob(stack)
			jobs[java.RunJobName] = java.RunJob(stack)
			jobs[java.PackageDockerJobName] = java.PackageDockerJob(stack)
			jobs[java.PublishDockerJobName] = java.PublishDockerJob()
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", java.ExtensionName, err)
		return 1
	}
	return 0
}
