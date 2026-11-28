// Command putnami-go is the unified entry point for all Go extension jobs.
// It dispatches to subcommands: build, build-generate, test, lint, serve,
// package, config-extract, config-merge.
//
// Usage:
//
//	putnami-go <command> [flags]
//
// Commands:
//
//	build           Compile Go project
//	build-generate  Generate openapi/proto/etc. specs from project sources
//	test            Run Go tests with coverage
//	test-identity   Name the test binaries a test run builds, as test-exec's key
//	lint            Lint Go code with golangci-lint and staticcheck
//	validate-api    Check a stable project's exported API and command surface against its line's last tag
//	serve           Run Go application with hot-reload
//	run             Run a Go workload once and forward its exit code
//	package         Create distribution packages
//	workspace-fetch    Download the modules and the pinned dev tools, the one
//	                   job a hosted run gives the registry credential
//	workspace-install  Install the pinned dev tools with the provisioned Go (`putnami install`)
//	deps-upgrade       Upgrade the go.putnami.dev release lock (`putnami upgrade`)
//	project-setup      Set a created Go project up (`putnami projects create`)
//	deps-edit          Edit a Go module's requirements (`putnami deps add|remove|prune`)
package main

import (
	"fmt"
	"io"
	"os"
	"slices"

	"go.putnami.dev/go/extension/internal/gocacheprog"
	"go.putnami.dev/go/extension/internal/jobs/cachepolicy"
	"go.putnami.dev/go/extension/internal/jobs/depsedit"
	"go.putnami.dev/go/extension/internal/jobs/depsupgrade"
	"go.putnami.dev/go/extension/internal/jobs/projectsetup"
	"go.putnami.dev/go/extension/internal/jobs/workspaceinstall"
	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/runtimeinfo"
)

// commandHandlers is the job dispatch table. Every entry that can reach the Go
// module origin over the network is wrapped so the user's registry credential is
// refreshed first — see withRegistryCredential for why the table is the right
// place for that.
//
// The table is the union of two halves, each in a file of its own: the
// codegen jobs (handlers_codegen.go) and every other job
// (handlers_general.go). bin/prepare builds the runtime's two executables
// from this one package: compiled/putnami-go with the putnami_runtime_main tag,
// which leaves the codegen half out, and the codegen entry
// compiled/putnami-go-codegen with the putnami_runtime_codegen tag, which
// leaves the other half out. A task keys on the bytes of the executable it
// runs (runtime.compiledInputs), so an edit of the codegen half re-keys
// build-generate and build-describe alone, and an edit of the other half
// leaves them alone. An untagged build, the one tests and linters see, holds
// both halves.
func commandHandlers() map[string]cli.JobFunc {
	handlers := generalHandlers()
	for name, job := range codegenHandlers() {
		handlers[name] = job
	}
	wrapped := make(map[string]cli.JobFunc, len(handlers))
	for name, job := range handlers {
		wrapped[name] = withRemoteBuildCacheOption(withDeclaredRegistries(withRegistryCredential(name, job)))
	}
	return wrapped
}

// lifecycleHandlers are the workspace lifecycle jobs: `putnami install` runs
// workspace-install, `putnami upgrade` runs deps-upgrade, `putnami projects
// create` runs project-setup and `putnami deps add|remove|prune` run
// deps-edit. A run without a project is a run for the workspace, never a SKIP,
// and no job gets the registry-credential refresh of the build jobs:
// project-setup refreshes the credential of a framework module's origin itself,
// and only when it reads that origin. On a hosted run the engine runs
// workspace-fetch before workspace-install, and hands it, and only it, the
// read credential.
func lifecycleHandlers() map[string]cli.JobFunc {
	return map[string]cli.JobFunc{
		"workspace-fetch":   workspaceinstall.RunFetch,
		"workspace-install": workspaceinstall.Run,
		"deps-upgrade":      depsupgrade.Run,
		"project-setup":     projectsetup.Job(refreshHostCredential),
		"deps-edit":         depsedit.Run,
	}
}

// lifecycleUsage is the help line of each lifecycle job, and the arguments
// that take a value in its argument list.
var lifecycleUsage = map[string]struct {
	usage  string
	valued []string
}{
	"workspace-fetch":   {usage: workspaceinstall.FetchUsage, valued: []string{"--putnamiContext", "--output"}},
	"workspace-install": {usage: workspaceinstall.Usage, valued: []string{"--putnamiContext", "--output"}},
	"deps-upgrade":      {usage: depsupgrade.Usage, valued: []string{"--putnamiContext", "--output", "--version", "--channel"}},
	"project-setup":     {usage: projectsetup.Usage, valued: []string{"--putnamiContext", "--output"}},
	"deps-edit":         {usage: depsedit.Usage, valued: []string{"--putnamiContext", "--output"}},
}

// lifecycleHelp returns the usage line to print when args ask a lifecycle job
// for help: --help or -h in a position its argument list reads as a flag, as
// the scripts read it, with or without a job context.
func lifecycleHelp(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	command, ok := lifecycleUsage[args[0]]
	if !ok {
		return "", false
	}
	for i := 1; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--help" || arg == "-h":
			return command.usage, true
		case slices.Contains(command.valued, arg):
			i++
		}
	}
	return "", false
}

// dispatchOptions are the cli.Run options of the job args name.
func dispatchOptions(args []string) []cli.RunOption {
	if len(args) > 0 {
		if _, ok := lifecycleHandlers()[args[0]]; ok {
			return []cli.RunOption{cli.WithSkipEmptyProject(false)}
		}
	}
	return nil
}

func runEntrypoint(
	args []string,
	stdout io.Writer,
	getenv func(string) string,
	runSubcommands func(map[string]cli.JobFunc),
) error {
	// The runtime carries no version of its own: it reports the version of the
	// manifest that declares it, so one build serves every version stamp.
	if handled, err := runtimeinfo.HandleFromManifest(args, stdout, goExtensionName); handled {
		return err
	}

	// The workspace probe is a reserved CONTROL call, not a job: core spawns it
	// with one request on stdin and expects one result document on stdout, with
	// no job context and no result file. It is handled beside the runtime
	// handshake, before cli.RunSubcommand, because that dispatcher would demand
	// a --putnamiContext the probe deliberately does not have. A failure returns
	// an error and writes nothing to stdout, so core sees "no result document"
	// rather than a partial answer it might merge.
	if handled, err := handleWorkspaceProbe(args, os.Stdin, stdout); handled {
		if err != nil {
			return fmt.Errorf("workspace-probe: %w", err)
		}
		return nil
	}
	// The toolchain-resolve call is a control call of the same shape: one
	// request on stdin, one result on stdout, nothing on stdout on failure.
	if handled, err := handleToolchainResolve(args, os.Stdin, stdout, getenv); handled {
		if err != nil {
			return fmt.Errorf("toolchain-resolve: %w", err)
		}
		return nil
	}
	// The input-resolve call is a control call of the same shape.
	if handled, err := handleInputResolve(args, os.Stdin, stdout); handled {
		if err != nil {
			return fmt.Errorf("input-resolve: %w", err)
		}
		return nil
	}

	// The GOCACHEPROG helper is spawned by the `go` command, not by core and
	// not by a user: toolchain.GoCommandEnv points GOCACHEPROG at this binary
	// plus this argument. It owns stdin and stdout for the lifetime of that go
	// invocation — one JSON object per line, nothing else — so it is handled
	// here, ahead of a dispatcher that would look for a job context it has no
	// way to carry.
	if len(args) > 0 && args[0] == toolchain.GoCacheProgSubcommand {
		return gocacheprog.Run(os.Stdin, stdout, os.Stderr, getenv)
	}

	// The reserved cache commands are workspace-level and consume the
	// environment (PUTNAMI_EXTENSION_CACHE_ROOT and the Go cache overrides)
	// rather than the per-job --putnamiContext contract, so they are handled
	// before cli.RunSubcommand — which would demand a project context and SKIP
	// on a workspace-level command.
	if len(args) > 0 {
		switch args[0] {
		case cachepolicy.PhaseClean, cachepolicy.PhaseGC:
			return cachepolicy.Run(args[0], getenv, stdout)
		}
	}

	if usage, ok := lifecycleHelp(args); ok {
		fmt.Fprintln(os.Stderr, usage)
		return nil
	}

	commands := commandHandlers()
	for name, job := range lifecycleHandlers() {
		commands[name] = job
	}
	runSubcommands(commands)
	return nil
}

func main() {
	err := runEntrypoint(os.Args[1:], os.Stdout, os.Getenv, func(commands map[string]cli.JobFunc) {
		cli.RunSubcommand(commands, dispatchOptions(os.Args[1:])...)
	})
	if err != nil {
		// Human output goes to stderr, never stdout: a control call's stdout
		// carries exactly one result document, and a message printed there
		// would make the caller reject a well-formed answer as trailing data.
		fmt.Fprintf(os.Stderr, "putnami-go: %v\n", err)
		os.Exit(1)
	}
}
