// Command putnami-php is the runtime of @putnami/php: the stack core run
// with the PHP profile and the Composer detector.
package main

import (
	"fmt"
	"io"
	"os"

	php "go.putnami.dev/php/extension"
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
	stack, err := php.Stack()
	if err == nil {
		var handled bool
		if handled, err = php.HandleToolchainResolve(args, stdin, stdout, stack.Profile); !handled && err == nil {
			err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
				jobs[php.InstallJobName] = php.InstallJob(stack)
				runJobs(jobs)
			})
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", php.ExtensionName, err)
		return 1
	}
	return 0
}
