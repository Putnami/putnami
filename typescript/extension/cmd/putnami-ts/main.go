// putnami-ts is the Go-native TypeScript extension binary for Putnami.
// It provides subcommands for build, test, lint, serve, and package operations.
package main

import (
	"context"
	"fmt"
	"os"

	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/sdk/extension/docslinks"
	"go.putnami.dev/sdk/extension/runtimeinfo"
	"go.putnami.dev/typescript/extension/internal/cachepolicy"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

var runtimeVersion string

var commands = map[string]cli.JobFunc{
	"build":           runBuild,
	"build-generate":  runBuildGenerate,
	"build-transpile": runBuildTranspile,
	"build-types":     runBuildTypes,
	"build-compile":   runBuildCompile,
	"build-infra":     buildInfra(),
	"config-extract":  runConfigExtract,
	// The documentation link check is the SDK's shared task body, so a broken
	// link fails lint the same way in every language.
	"lint-docs": docslinks.Job(),
	// The database test environment is the SDK's shared task body: policy, closure discovery, the Postgres container and the
	// invocation-scoped sensitive binding are identical for every language, so
	// three copies of them would drift.
	"test-env-up":       dbtestenv.UpJob(),
	"test-env-down":     dbtestenv.DownJob(),
	"test":              runTest,
	"lint":              runLint,
	"lint-format":       runLintFormat,
	"lint-check":        runLintCheck,
	"serve":             runServe,
	"run":               runRun,
	"package-npm":       runPackageNpm,
	"package-docker":    runPackageDocker,
	"publish-npm":       runPublishNpm,
	"publish-docker":    runPublishDocker,
	"workspace-fetch":   runWorkspaceFetch,
	"workspace-install": runWorkspaceInstall,
	"workspace-sync":    runWorkspaceSync,
	"deps-upgrade":      runDepsUpgrade,
	"version":           runVersion,
}

func main() {
	// This extension owns its own Bun cache location since an earlier migration:
	// core no longer exports language cache variables into every job, so the
	// Putnami-side override has to be translated into the one bun reads here,
	// once, before anything spawns bun.
	toolchain.ApplyBunCacheEnv()
	// A bun that Putnami installed keeps its files under its install
	// directory, which the CLI names in BUN_INSTALL for the task.
	toolchain.ApplyManagedBunEnv()

	if handled, err := runtimeinfo.Handle(os.Args[1:], os.Stdout, tsExtensionName, runtimeVersion); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == mcpToolArg {
		if err := runMCPTool(context.Background(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "putnami-ts: %v\n", err)
			os.Exit(1)
		}
		return
	}
	// The workspace probe is a reserved CONTROL call, not a job: core spawns it
	// with one request on stdin and expects one result document on stdout, with
	// no job context and no result file. It is handled beside the runtime
	// handshake, before cli.RunSubcommand, because that dispatcher would demand
	// a --putnamiContext the probe deliberately does not have. Failures print to
	// stderr and exit non-zero, so core sees "no result document" rather than a
	// partial answer it might merge.
	if handled, err := handleWorkspaceProbe(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "putnami-ts: workspace-probe: %v\n", err)
			os.Exit(1)
		}
		return
	}
	// The reserved cache commands are workspace-level and consume the
	// environment (PUTNAMI_CACHE_ROOT, PUTNAMI_EXTENSION_CACHE_ROOT and the Bun
	// cache overrides) rather than the per-job --putnamiContext contract, so
	// they are handled before cli.RunSubcommand — which would demand a project
	// context a workspace-level command does not have.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case cachepolicy.PhaseClean, cachepolicy.PhaseGC:
			err := cachepolicy.Run(os.Args[1], os.Getenv("PUTNAMI_CACHE_ROOT"), os.Getenv, os.Stdout)
			if err != nil {
				os.Exit(1)
			}
			return
		}
	}
	cli.RunSubcommand(commands)
}
