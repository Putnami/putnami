// Command putnami-bazel is the runtime of @putnami/bazel: the stack core run
// with the Bazel profile and the Bazel detector, the workspace sync task that
// writes each package's putnami.json, and the command that collects the
// reports of a test run.
package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/tooling/bazel"
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
	stack, err := bazel.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs[bazel.SyncJob] = bazel.RunSync
			jobs[bazel.CheckJob] = bazel.RunCheck
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", bazel.ExtensionName, err)
		return 1
	}
	return 0
}
