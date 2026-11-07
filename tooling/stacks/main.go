package stacks

import (
	"fmt"
	"io"
	"os"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/runtimeinfo"
)

// Main runs a stack extension's runtime: the runtime handshake, the workspace
// probe and the input-resolve control calls, then the lint, test and build
// jobs. version is the runtime version the build stamps.
func Main(s Stack, version string) {
	if err := s.Entrypoint(os.Args[1:], os.Stdin, os.Stdout, version, func(jobs map[string]cli.JobFunc) {
		cli.RunSubcommand(jobs, cli.WithSkipEmptyProject(false))
	}); err != nil {
		// A control call's stdout carries exactly one document: errors go to
		// stderr.
		fmt.Fprintf(os.Stderr, "%s: %v\n", s.Extension, err)
		os.Exit(1)
	}
}

// Entrypoint dispatches one invocation; runJobs runs the job dispatcher.
func (s Stack) Entrypoint(args []string, stdin io.Reader, stdout io.Writer, version string, runJobs func(map[string]cli.JobFunc)) error {
	if handled, err := runtimeinfo.Handle(args, stdout, s.Extension, version); handled {
		return err
	}
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve the workspace root: %w", err)
	}
	if handled, err := wsproto.ServeProbe(args, stdin, stdout, func(r wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
		return s.Probe(root, r)
	}); handled {
		if err != nil {
			return fmt.Errorf("workspace-probe: %w", err)
		}
		return nil
	}
	if handled, err := wsproto.ServeInputResolve(args, stdin, stdout, func(r wsproto.InputResolveRequest) (wsproto.InputResolveResult, error) {
		return s.ResolveInputs(root, r), nil
	}); handled {
		if err != nil {
			return fmt.Errorf("input-resolve: %w", err)
		}
		return nil
	}
	runJobs(s.Jobs())
	return nil
}

// Jobs are the job handlers of the profile's tasks.
func (s Stack) Jobs() map[string]cli.JobFunc {
	jobs := map[string]cli.JobFunc{}
	for _, task := range TaskNames {
		if _, ok := s.Profile.Tasks[task]; ok {
			jobs[task] = s.Run(task)
		}
	}
	return jobs
}
