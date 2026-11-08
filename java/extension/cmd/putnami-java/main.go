// Command putnami-java is the runtime of @putnami/java: the stack core run
// with the Java profile and the Gradle and Maven detectors.
package main

import (
	"fmt"
	"io"
	"os"

	java "go.putnami.dev/java/extension"
	"go.putnami.dev/sdk/extension/cli"
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
		var handled bool
		if handled, err = java.HandleToolchainResolve(args, stdin, stdout, stack.Profile); !handled && err == nil {
			err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
				jobs[java.ServeJobName] = java.ServeJob(stack)
				jobs[java.RunJobName] = java.RunJob(stack)
				jobs[java.PackageDockerJobName] = java.PackageDockerJob(stack)
				jobs[java.PublishDockerJobName] = java.PublishDockerJob()
				runJobs(jobs)
			})
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", java.ExtensionName, err)
		return 1
	}
	return 0
}
