// Command putnami-ruby is the runtime of @putnami/ruby: the stack core run
// with the Ruby profile and the Ruby detector.
package main

import (
	"fmt"
	"io"
	"os"

	ruby "go.putnami.dev/ruby/extension"
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
	stack, err := ruby.Stack()
	if err == nil {
		var handled bool
		if handled, err = ruby.HandleToolchainResolve(args, stdin, stdout, stack.Profile); !handled && err == nil {
			err = stack.Entrypoint(args, stdin, stdout, runtimeVersion, func(jobs map[string]cli.JobFunc) {
				jobs[stacks.TaskTest] = ruby.TestJob(stack)
				jobs["lint-docs"] = docslinks.Job("Gemfile", "package.yml")
				jobs[ruby.InstallJobName] = ruby.InstallJob(stack)
				jobs[ruby.WorkspaceFetchJobName] = ruby.WorkspaceFetchJob(stack)
				jobs[ruby.WorkspaceInstallJobName] = ruby.WorkspaceInstallJob(stack)
				jobs[ruby.ServeJobName] = ruby.ServeJob(stack)
				jobs[ruby.RunJobName] = ruby.RunJob(stack)
				jobs[ruby.DepsUpgradeJobName] = ruby.DepsUpgradeJob(stack)
				jobs[ruby.PackageDockerJobName] = ruby.PackageDockerJob(stack)
				jobs[ruby.PublishDockerJobName] = ruby.PublishDockerJob()
				runJobs(jobs)
			})
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", ruby.ExtensionName, err)
		return 1
	}
	return 0
}
