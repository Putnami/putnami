// Command putnami-moon is the runtime of @putnami/moon: the stack core run
// with the moon profile and the moon detector, the workspace sync task that
// writes each moon project's putnami.json, and the validate task that fails
// on a stale project graph.
package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/monorepo/moon"
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
	stack, err := moon.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs[moon.SyncJob] = moon.RunSync
			jobs[moon.CheckJob] = moon.RunCheck
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", moon.ExtensionName, err)
		return 1
	}
	return 0
}
