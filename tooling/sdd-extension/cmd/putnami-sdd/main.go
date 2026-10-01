// Command putnami-sdd is the single entry point of the @putnami/sdd extension.
//
// Every manifest task and every interactive subcommand names this binary as its
// command and selects work with its leading positional arguments, so the
// process boundary between the CLI and SDD is one executable and two dispatch
// tables.
//
// Usage:
//
//	putnami-sdd <job> [flags]
//	putnami-sdd <group> <subcommand> [args] [flags]
//	putnami-sdd mcp-tool   (one ToolCallRequest on stdin, one ToolCallResult on stdout)
//
// Jobs (the DAG's):
//
//	selfcheck              Report the runtime and the job context this extension receives
//	features-validate      Validate one project's durable feature manifest and its evidence
//	specs-validate         Validate one project's specs against the features they detail
//	specs-ratchet-validate Compare the enforced-spec floor against the committed baseline
//	architecture-validate  Validate the workspace's ARC/DARC declarations and project graph
//	decisions-validate     Prove the workspace's committed decisions.json registries against the worktree
//	recipes-validate       Check every <lang>/samples/recipes.json against the worktree
//	codeowners-sync        Write .github/CODEOWNERS from the owners each putnami.json declares
//	docs-links-validate    Check the links of every README.md file and doc/ tree of the workspace
//
// Command groups (the interactive surface):
//
//	features      list | validate | snapshot | inspect | diff
//	specs         list | validate | verify | baseline | inspect | init
//	architecture  validate | snapshot | inspect
//	contracts     generate | check
//
// MCP tools (the agent surface): sdd.list_features,
// sdd.feature_context, sdd.list_specs, sdd.spec_context and
// sdd.architecture_context, all five answered by the single `mcp-tool`
// argument.
//
// The two tables answer the same questions and must never answer them
// differently, which is why both call the SAME engine builders in internal/sdd
// and neither holds a verdict of its own.
//
// They differ in ONE thing, and it is the reason they are two tables: what
// stdout means. A job's stdout is the JSONL runtime-event stream the CLI parses
// (cli.Run writes it); an interactive subcommand's stdout is the terminal's, and
// what it writes is the command's own output — a ResultV2 envelope under
// --output=json, human text otherwise. Routing an interactive subcommand
// through cli.Run would put a meta event beside the envelope and break every
// consumer that pipes it.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/runtimeinfo"
)

// extensionName is the identity this executable answers the runtime handshake
// with. It must stay equal to the manifest's `name` member: core compares the
// descriptor a prepared runtime returns against the manifest that declared it,
// so a drift here is a runtime that is rejected as somebody else's.
const extensionName = "@putnami/sdd"

// runtimeVersion is stamped at link time. putnami.json declares
// options["@putnami/go"].version-var = "main.runtimeVersion", which is what the
// Go extension's packager passes to -ldflags -X; an unstamped build (a plain
// `go build`, or `go test`) leaves it empty, and the handshake then reports an
// empty version rather than lying about one.
var runtimeVersion string

// commandHandlers is the dispatch table: subcommand name → job body.
//
// It is a function rather than a package-level map so a test can compare the
// table to the manifest without one test's mutation reaching another's.
func commandHandlers() map[string]cli.JobFunc {
	return map[string]cli.JobFunc{
		"selfcheck":              runSelfcheck,
		"features-validate":      runFeaturesValidate,
		"specs-validate":         runSpecsValidate,
		"specs-ratchet-validate": runSpecsRatchetValidate,
		"architecture-validate":  runArchitectureValidate,
		"decisions-validate":     runDecisionsValidate,
		"recipes-validate":       runRecipesValidate,
		"codeowners-sync":        runCodeownersSync,
		"docs-links-validate":    runDocsLinksValidate,
	}
}

// runEntrypoint is main with its process-level dependencies — the argument
// vector, the two streams, and the job dispatcher — passed in, so the reserved
// handshake and the interactive routing are testable without spawning the
// binary. It returns the process exit code beside the error.
//
// The order of the three branches is the contract:
//
//  1. The runtime handshake first, and it has to be: core spawns
//     `__putnami runtime-info` with no --putnamiContext, and cli.RunSubcommand
//     would reject that as a usage error before the descriptor was ever written.
//  2. The interactive command groups next, because their stdout is the
//     terminal's and must carry their own document — never cli.Run's JSONL.
//  3. The MCP tool bridge, whose stdout carries one ToolCallResult and whose
//     stdin carries the request, so it can never go through cli.Run either.
//  4. Everything else is a job.
func runEntrypoint(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	runSubcommands func(map[string]cli.JobFunc),
) (int, error) {
	if handled, err := runtimeinfo.Handle(args, stdout, extensionName, runtimeVersion); handled {
		return 0, err
	}

	if sub, rest, found := lookupInteractiveSubcommand(args); found {
		return runInteractiveSubcommand(sub, rest, stdout, stderr), nil
	}

	if len(args) > 0 && args[0] == mcpToolArg {
		return 0, runMCPTool(context.Background(), stdin, stdout)
	}

	runSubcommands(commandHandlers())
	return 0, nil
}

func main() {
	code, err := runEntrypoint(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, func(commands map[string]cli.JobFunc) {
		// SDD work is workspace-shaped as often as it is project-shaped
		// (`architecture` is workspace-once), so an empty project name is not a
		// reason to skip: the job body decides.
		cli.RunSubcommand(commands, cli.WithSkipEmptyProject(false))
	})
	if err != nil {
		// Human output goes to stderr, never stdout: a control call's stdout
		// carries exactly one document, and a message printed beside it would
		// make core reject a well-formed answer as trailing data.
		fmt.Fprintf(os.Stderr, "putnami-sdd: %v\n", err)
		os.Exit(1)
	}
	if code != 0 {
		os.Exit(code)
	}
}
