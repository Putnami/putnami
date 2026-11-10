// Command putnami-nx is the runtime of @putnami/nx: the stack core run
// with the Nx profile and the Nx detector, and the workspace sync task
// that writes each project's putnami.json.
package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/integrations/nx"
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
	stack, err := nx.Stack()
	if err == nil {
		err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
			jobs[nx.SyncJob] = nx.RunSync
			jobs[nx.CheckJob] = nx.RunCheck
			runJobs(jobs)
		})
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", nx.ExtensionName, err)
		return 1
	}
	return 0
}
