// Package cli provides the standard job entry point and flag parsing for
// Putnami extension binaries.
//
// Every job accepts --putnamiContext plus the reserved output flags --output
// and --json, resolved through the shared CLI contract in
// go.putnami.dev/protocol/cli so extensions honor the same output modes,
// exit-code taxonomy, and error classification as the framework CLI. This
// package provides the common parsing logic and Run/RunSubcommand helpers that
// handle context loading, meta emission, and result reporting.
//
// Extensions always emit their events as JSONL on stdout; the framework CLI
// owns aggregation of --output=json, so the SDK validates the output mode but
// does not switch its own stdout format on it.
package cli

import (
	"fmt"
	"os"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Result status strings emitted on the JSONL wire (the runtime event protocol).
const (
	statusFailed = "FAILED"
	statusSkip   = "SKIP"
)

// JobFunc is the function signature for job implementations.
//
// Error contract: an implementation MUST return a non-nil error whenever it
// reports a "FAILED" status, so programmatic callers can rely on the returned
// error rather than parsing the status string. Run treats either a non-nil
// error or a "FAILED" status as a failure.
//
// Exit codes: the returned error's classification selects the process exit code
// via the shared taxonomy. Wrap the error with the go.putnami.dev/protocol/cli
// classifiers (e.g. cli.Authf, cli.APIf, or cli.Classify(err, cli.ErrAuth)) to
// exit with auth (3) or api (4); an unclassified error exits with failure (1).
type JobFunc func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (status string, data map[string]any, err error)

// RunOption configures Run behavior.
type RunOption func(*runOptions)

type runOptions struct {
	skipEmptyProject bool
}

// WithSkipEmptyProject controls whether Run automatically emits SKIP and exits
// when the project name is empty. Default: true.
// Set to false for extensions with workspace-level jobs (e.g., Python, CI).
func WithSkipEmptyProject(skip bool) RunOption {
	return func(o *runOptions) { o.skipEmptyProject = skip }
}

// RunSubcommand dispatches to one of several named jobs based on the first
// positional argument (the subcommand). All remaining arguments are forwarded
// to the selected job via Run.
func RunSubcommand(commands map[string]JobFunc, opts ...RunOption) {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: <binary> <command> [flags]")
		fmt.Fprintln(os.Stderr, "Commands:")
		for name := range commands {
			fmt.Fprintf(os.Stderr, "  %s\n", name)
		}
		os.Exit(protocolcli.ExitUsage)
	}

	subcmd := os.Args[1]
	job, ok := commands[subcmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", subcmd) //nolint:gosec // CLI stderr; not an HTTP response
		os.Exit(protocolcli.ExitUsage)
	}

	// Shift args so Run sees flags starting at os.Args[1].
	os.Args = append(os.Args[:1], os.Args[2:]...)
	Run(job, opts...)
}

// Run is the standard entry point for all extension jobs.
// It parses --putnamiContext, loads the context, emits meta, runs the job,
// and emits the result.
func Run(job JobFunc, opts ...RunOption) {
	o := &runOptions{skipEmptyProject: true}
	for _, opt := range opts {
		opt(o)
	}

	flags, err := parseStandardFlags(os.Args[1:])
	if err != nil {
		// Bad --output/--json is a usage error; exit via the shared taxonomy.
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(protocolcli.ExitCodeForError(err))
	}
	// flags.mode is validated here; extensions always emit JSONL and the
	// framework CLI owns --output=json aggregation, so nothing else uses it.
	_ = flags.mode

	if flags.contextFile == "" {
		fmt.Fprintln(os.Stderr, "Error: --putnamiContext is required")
		os.Exit(protocolcli.ExitUsage)
	}

	ctx, err := pctx.Parse(flags.contextFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(protocolcli.ExitUsage)
	}

	emit := jsonl.New()

	if o.skipEmptyProject && ctx.Project.Name == "" {
		emit.Result(statusSkip, nil)
		os.Exit(protocolcli.ExitSuccess)
	}

	emit.Meta(ctx.Extension.Name, ctx.Job.Name)

	status, data, jobErr := job(ctx, emit, flags.jobArgs)
	if jobErr != nil {
		emit.Error(jobErr.Error())
		emit.Result(statusFailed, nil)
		os.Exit(exitCodeFor(statusFailed, jobErr))
	}
	emit.Result(status, data)
	if code := exitCodeFor(status, nil); code != protocolcli.ExitSuccess {
		os.Exit(code)
	}
	// Success: return so the caller's main exits 0 normally.
}

// standardFlags holds the CLI-contract flags every extension job accepts,
// extracted from the raw argument list before the job runs.
type standardFlags struct {
	contextFile string
	mode        protocolcli.OutputMode
	jobArgs     []string
}

// parseStandardFlags pulls --putnamiContext and the reserved output flags
// (--output / --json, in both "--flag value" and "--flag=value" forms) out of
// args, resolving the output mode through the shared CLI contract. Every other
// argument flows through to the job unchanged. A --json/--output conflict or an
// unknown --output value is returned as an ErrUsage-classified error.
func parseStandardFlags(args []string) (standardFlags, error) {
	var (
		contextFile string
		rawOutput   string
		jsonFlag    bool
		jobArgs     []string
	)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--putnamiContext":
			if i+1 < len(args) {
				contextFile = args[i+1]
				i++
			}
		case arg == "--output":
			if i+1 < len(args) {
				rawOutput = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--output="):
			rawOutput = strings.TrimPrefix(arg, "--output=")
		case arg == "--json":
			jsonFlag = true
		default:
			jobArgs = append(jobArgs, arg)
		}
	}

	mode, err := protocolcli.ResolveOutputMode(rawOutput, jsonFlag)
	if err != nil {
		return standardFlags{}, protocolcli.Classify(err, protocolcli.ErrUsage)
	}
	return standardFlags{contextFile: contextFile, mode: mode, jobArgs: jobArgs}, nil
}

// exitCodeFor maps a job's (status, error) outcome to a process exit code via
// the shared CLI exit-code taxonomy. A returned error's classification wins
// (auth→3, api→4, usage→2, otherwise failure→1); a bare "FAILED" status with no
// error is a plain failure; anything else is success.
func exitCodeFor(status string, err error) int {
	if err != nil {
		return protocolcli.ExitCodeForError(err)
	}
	if status == statusFailed {
		return protocolcli.ExitFailure
	}
	return protocolcli.ExitSuccess
}
