package stacks

import (
	"fmt"
	"io"
	"os"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
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

// TaskGraph is the job of a delegated stack's graph step: it runs nothing and
// passes. Each lint, test and build step of a delegated stack's manifest
// depends on the graph step of its own project, which alone depends on the
// graph step of each project upstream: a change selects its dependents, and
// a dependency runs its graph step, never its tool.
const TaskGraph = "graph"

// Jobs are the job handlers of the profile's tasks, and the graph job of a
// delegated profile.
func (s Stack) Jobs() map[string]cli.JobFunc {
	jobs := map[string]cli.JobFunc{}
	if s.Profile.Delegated {
		jobs[TaskGraph] = func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error) {
			return statusOK, nil, nil
		}
	}
	for _, task := range TaskNames {
		if s.Profile.HasTask(task) {
			jobs[task] = s.Run(task)
		}
	}
	return jobs
}
