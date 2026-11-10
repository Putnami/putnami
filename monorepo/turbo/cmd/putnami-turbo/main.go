// Command putnami-turbo is the runtime of @putnami/turbo: the stack core run
// with the Turborepo profile and detector, the workspace sync task that
// writes each package's putnami.json, and the check of the package graph.
package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/monorepo/turbo"
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
	stack, err := turbo.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs[turbo.SyncJob] = turbo.RunSync
			jobs[turbo.CheckJob] = turbo.RunCheck
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", turbo.ExtensionName, err)
		return 1
	}
	return 0
}
