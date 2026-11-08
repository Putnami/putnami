package stacks

import (
	"fmt"
	"io"
	"os"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/runtimeinfo"
)

// Entrypoint dispatches one invocation of a stack extension's runtime: the
// runtime handshake, the workspace probe and the input-resolve control calls,
// then the lint, test and build jobs through runJobs, the SDK's job
// dispatcher. version is the runtime version the build stamps. A control
// call's stdout carries exactly one document, so the caller writes a returned
// error to stderr.
func (s Stack) Entrypoint(args []string, stdin io.Reader, stdout io.Writer, version string, runJobs func(map[string]cli.JobFunc)) error {
	if handled, err := runtimeinfo.Handle(args, stdout, s.Extension, version); handled {
		return err
	}
	if len(args) > 0 {
		if command, ok := s.Commands[args[0]]; ok {
			if err := command(args[1:], stdout, os.Stderr); err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			return nil
		}
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
		if s.Profile.HasTask(task) {
			jobs[task] = s.Run(task)
		}
	}
	return jobs
}
