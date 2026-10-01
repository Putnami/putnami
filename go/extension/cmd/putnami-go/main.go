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
//	lint            Lint Go code with golangci-lint and staticcheck
//	serve           Run Go application with hot-reload
//	run             Run a Go workload once and forward its exit code
//	package         Create distribution packages
//	workspace-fetch    Download Go, the modules and the pinned dev tools, the one
//	                   job a hosted run gives the registry credential
//	workspace-install  Install Go and the pinned dev tools (`putnami install`)
//	deps-upgrade       Upgrade the go.putnami.dev release lock (`putnami upgrade`)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"go.putnami.dev/go/extension/internal/codegen"
	"go.putnami.dev/go/extension/internal/gocacheprog"

	// Blank-imported visitors register themselves with the codegen package.
	// Add a new generator by introducing a sibling subpackage and listing it
	// here — no other plumbing is required.
	_ "go.putnami.dev/go/extension/internal/codegen/openapi"
	"go.putnami.dev/go/extension/internal/jobs/build"
	"go.putnami.dev/go/extension/internal/jobs/cachepolicy"
	"go.putnami.dev/go/extension/internal/jobs/configextract"
	"go.putnami.dev/go/extension/internal/jobs/configmerge"
	"go.putnami.dev/go/extension/internal/jobs/depsupgrade"
	"go.putnami.dev/go/extension/internal/jobs/lint"
	"go.putnami.dev/go/extension/internal/jobs/pkg"
	"go.putnami.dev/go/extension/internal/jobs/publish"
	"go.putnami.dev/go/extension/internal/jobs/run"
	"go.putnami.dev/go/extension/internal/jobs/serve"
	"go.putnami.dev/go/extension/internal/jobs/test"
	"go.putnami.dev/go/extension/internal/jobs/workspaceinstall"
	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/sdk/extension/docslinks"
	"go.putnami.dev/sdk/extension/infraagg"
	"go.putnami.dev/sdk/extension/runtimeinfo"
)

var runtimeVersion string

// commandHandlers is the job dispatch table. Every entry that can reach the Go
// module origin over the network is wrapped so the user's registry credential is
// refreshed first — see withRegistryCredential for why the table is the right
// place for that.
func commandHandlers() map[string]cli.JobFunc {
	handlers := map[string]cli.JobFunc{
		"build":          build.Run,
		"build-generate": codegen.Run,
		"build-describe": codegen.RunDescribe,
		// Infra aggregation is the SDK's shared task body. Go passes no runtime compatibility hook: a Go workload serves
		// whatever the platform defaults to, so it constrains nothing.
		"build-infra": infraagg.Job(infraagg.Options{}),
		// The documentation link check is the SDK's shared task body too, so a
		// broken link fails lint the same way in every language.
		"lint-docs": docslinks.Job(),
		// The database test environment is the SDK's shared task body: policy,
		// closure discovery, the Postgres container and the invocation-scoped
		// sensitive binding are identical for every language, so three copies of
		// them would drift.
		"test-env-up":    dbtestenv.UpJob(),
		"test-env-down":  dbtestenv.DownJob(),
		"test":           test.Run,
		"lint":           lint.Run,
		"serve":          serve.Run,
		"run":            run.Run,
		"package":        pkg.Run,
		"publish":        publish.Run,
		"config-extract": configextract.Run,
		"config-merge":   configmerge.Run,
		"workspace-sync": runWorkspaceSync,
	}
	wrapped := make(map[string]cli.JobFunc, len(handlers))
	for name, job := range handlers {
		wrapped[name] = withRemoteBuildCacheOption(withDeclaredRegistries(withRegistryCredential(name, job)))
	}
	return wrapped
}

// lifecycleHandlers are the workspace lifecycle jobs: `putnami install` runs
// workspace-install and `putnami upgrade` runs deps-upgrade. They were shell
// scripts, and they keep the scripts' contract: no registry-credential
// refresh, and a run without a project is a run for the workspace, never a
// SKIP. On a hosted run the engine runs workspace-fetch before
// workspace-install, and hands it, and only it, the read credential.
func lifecycleHandlers() map[string]cli.JobFunc {
	return map[string]cli.JobFunc{
		"workspace-fetch":   workspaceinstall.RunFetch,
		"workspace-install": workspaceinstall.Run,
		"deps-upgrade":      depsupgrade.Run,
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
	if handled, err := runtimeinfo.Handle(args, stdout, goExtensionName, runtimeVersion); handled {
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
	if len(args) > 0 && args[0] == mcpToolArg {
		return runMCPTool(context.Background(), os.Stdin, stdout)
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
		fmt.Fprintln(os.Stderr, usage) //nolint:gosec // G705: usage is a constant of this binary, written to a terminal
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
