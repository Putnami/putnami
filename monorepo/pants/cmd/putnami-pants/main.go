// Command putnami-pants is the runtime of @putnami/pants: the stack core run
// with the Pants profile and the Pants detector, and the workspace sync task
// that writes each package's putnami.json.
package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/tooling/pants"
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
	stack, err := pants.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs[pants.SyncJob] = pants.RunSync
			jobs[pants.CheckJob] = pants.RunCheck
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", pants.ExtensionName, err)
		return 1
	}
	return 0
}
