// Command putnami-python is the unified entry point for all Python extension jobs.
// It dispatches to subcommands: workspace-install, lint-format, lint-check, lint-docs, test, serve.
//
// Usage:
//
//	putnami-python <command> [flags]
//
// Commands:
//
//	workspace-install  Sync UV workspace and lock dependencies
//	workspace-sync     Align pyproject.toml identities with resolved project names
//	lint-format        Format Python code with ruff
//	lint-check         Check Python code with ruff
//	lint-docs          Check the links in README.md files and doc/ trees
//	test               Run Python tests with pytest
//	serve              Serve a Python application with hot-reload
//	run                Run a Python workload once and forward its exit code
package main

import (
	"fmt"
	"io"
	"os"

	"go.putnami.dev/python/extension/internal/jobs"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/docslinks"
	"go.putnami.dev/sdk/extension/runtimeinfo"
)

var runtimeVersion string

func commandHandlers() map[string]cli.JobFunc {
	return map[string]cli.JobFunc{
		"workspace-install": jobs.WorkspaceInstall,
		"workspace-sync":    runWorkspaceSync,
		"deps-upgrade":      jobs.DepsUpgrade,
		"lint-format":       jobs.LintFormat,
		"lint-check":        jobs.LintCheck,
		"lint-docs":         docslinks.Job(),
		"test":              jobs.Test,
		"serve":             jobs.Serve,
		"run":               jobs.Run,
	}
}

func runEntrypoint(
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	runSubcommands func(map[string]cli.JobFunc),
) error {
	if handled, err := runtimeinfo.Handle(args, stdout, pythonExtensionName, runtimeVersion); handled {
		return err
	}

	// The workspace probe is a reserved CONTROL call, not a job: core spawns it
	// with one request on stdin and expects one result document on stdout, with
	// no job context and no result file. It is handled beside the runtime
	// handshake, before cli.RunSubcommand, because that dispatcher would demand
	// a --putnamiContext the probe deliberately does not have. A failure returns
	// an error and writes nothing to stdout, so core sees "no result document"
	// rather than a partial answer it might merge.
	if handled, err := handleWorkspaceProbe(args, stdin, stdout); handled {
		if err != nil {
			return fmt.Errorf("workspace-probe: %w", err)
		}
		return nil
	}

	runSubcommands(commandHandlers())
	return nil
}

func main() {
	err := runEntrypoint(os.Args[1:], os.Stdin, os.Stdout, func(commands map[string]cli.JobFunc) {
		cli.RunSubcommand(commands, cli.WithSkipEmptyProject(false))
	})
	if err != nil {
		// Human output goes to stderr, never stdout: a control call's stdout
		// carries exactly one result document, and a message printed there
		// would make the caller reject a well-formed answer as trailing data.
		fmt.Fprintf(os.Stderr, "putnami-python: %v\n", err)
		os.Exit(1)
	}
}
