// Package qualifycmd is the `putnami qualify` vertical: it resolves one
// workload, derives its smoke contract, runs it against a target, and turns the
// verdict into the command's output and exit code.
//
// The exit code follows the verdict alone. passed exits 0; every other state
// exits 1 and still prints the verdict — under --output=json it travels as the
// failure envelope's data — so a caller never has to choose between the code
// and the evidence. Invalid invocations exit 2 before anything is derived.
package qualifycmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	qualifyproto "go.putnami.dev/protocol/qualify"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/qualify"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TargetLocal is the --target value that composes the workload on this machine.
const TargetLocal = "local"

// Options is one parsed `putnami qualify` invocation.
type Options struct {
	// WorkspaceRoot is the workspace the selector resolves in.
	WorkspaceRoot string
	// Selector names exactly one project: an ID, a name, or a path.
	Selector string
	// Target is "local" or an http(s) URL; empty only with PrintContract.
	Target string
	// ExpectSHA is the sha a URL target must report on /version.
	ExpectSHA string
	// PlatformPrefix is where /readyz and /version are mounted. Empty reads it
	// from the route inventory (qualify.ResolvePlatform).
	PlatformPrefix string
	// ReadyTimeout bounds readiness; zero means the default.
	ReadyTimeout time.Duration
	// RequestTimeout bounds each request; zero means the default.
	RequestTimeout time.Duration
	// PrintContract prints the derived contract and runs nothing.
	PrintContract bool
	// OutputFormat is the resolved --output value.
	OutputFormat string
	// Config is the loaded workspace configuration a local target composes
	// with; nil loads it.
	Config *wsproto.Config
	// Verbose renders a local composition's preparation and serve logs, on
	// stderr, in text output. Structured output never carries them: the verdict
	// is the record.
	Verbose bool
}

// VerdictError is the failure of a verdict that is not passed. It carries the
// verdict so the structured failure envelope still delivers it.
type VerdictError struct {
	// Verdict is the non-pass verdict.
	Verdict *qualifyproto.Verdict
}

func (e *VerdictError) Error() string {
	return fmt.Sprintf("qualify %s: verdict %s", e.Verdict.Project, e.Verdict.State)
}

// Run executes one invocation.
func Run(ctx context.Context, opts Options) error {
	if err := validate(opts); err != nil {
		return err
	}
	ws, err := workspace.Load(opts.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	project := shared.ResolveProjectSelector(ws, opts.Selector)
	if project == nil {
		return cmderr.NotFoundf("project not found: %s", opts.Selector)
	}
	// Without --platform-prefix, the route inventory names where /readyz and
	// /version are mounted, and whether /readyz is served at all: the same file
	// the contract is derived from.
	platform, err := qualify.ResolvePlatform(ws, project, opts.PlatformPrefix)
	if err != nil {
		var ambiguous *qualify.AmbiguousPlatformPrefixError
		if errors.As(err, &ambiguous) {
			return cmderr.Classify(err, cmderr.ErrInvalidConfig)
		}
		return err
	}
	contract, unsupported, err := qualify.Derive(ws, project, platform.Prefix)
	if err != nil {
		var invalid *qualify.InvalidInventoryError
		if errors.As(err, &invalid) {
			return cmderr.Classify(err, cmderr.ErrInvalidConfig)
		}
		return err
	}
	structured := output.StructuredOutput(opts.OutputFormat)
	if opts.PrintContract {
		if structured {
			return printJSON(contract)
		}
		return qualify.RenderContractText(iox.Stdout(), contract, unsupported)
	}

	target, err := newTarget(opts, project, structured)
	if err != nil {
		return err
	}
	verdict := qualify.Execute(ctx, contract, target, qualify.Options{
		PlatformPrefix: platform.Prefix,
		ReadinessRoute: platform.ReadinessRoute,
		ReadyTimeout:   opts.ReadyTimeout,
		RequestTimeout: opts.RequestTimeout,
		Unsupported:    unsupported,
	})
	var verdictErr error
	if !verdict.State.IsPass() {
		verdictErr = &VerdictError{Verdict: verdict}
	}
	if structured {
		if verdictErr != nil {
			// The dispatcher discards a failing command's stdout and writes the
			// failure envelope, so the verdict rides in its data member.
			return shared.WithResultData(verdictErr, verdict)
		}
		return printJSON(verdict)
	}
	if err := qualify.RenderVerdictText(iox.Stdout(), verdict); err != nil {
		return err
	}
	return verdictErr
}

// newTarget builds the target --target names. A local target fingerprints the
// worktree here, before anything is prepared or started.
func newTarget(opts Options, project *workspace.Project, structured bool) (qualify.Target, error) {
	if opts.Target != TargetLocal {
		target, err := qualify.NewURLTarget(opts.Target, opts.ExpectSHA)
		if err != nil {
			return nil, cmderr.Usagef("%v", err)
		}
		return target, nil
	}
	serveLogs, preparation := compositionRenderers(opts, structured)
	return qualify.NewLocalTarget(qualify.LocalOptions{
		WorkspaceRoot: opts.WorkspaceRoot,
		Config:        opts.Config,
		Project:       project,
		ReadyTimeout:  opts.ReadyTimeout,
		Output:        serveLogs,
		Preparation:   preparation,
	})
}

// compositionRenderers choose what a local composition writes. Only a verbose
// human run sees the members' serve logs and the preparation's task output, on
// stderr so stdout stays the verdict. Otherwise serve logs are dropped and the
// preparation reports only its failures, on stderr.
func compositionRenderers(opts Options, structured bool) (serveLogs, preparation jobs.Renderer) {
	if structured || !opts.Verbose {
		return nil, nil
	}
	serveLogs = output.NewTextRenderer(os.Stderr, os.Stderr, output.TextRendererConfig{ServeMode: true, Verbose: true})
	preparation = output.NewTextRenderer(os.Stderr, os.Stderr, output.TextRendererConfig{Verbose: true})
	return serveLogs, preparation
}

// validate refuses an invocation that cannot run before any file is read.
func validate(opts Options) error {
	prefix := opts.PlatformPrefix
	if strings.ContainsAny(prefix, "?#% \t") || strings.Contains(prefix, "://") {
		return cmderr.Usagef("--platform-prefix %q must be a plain path such as /_ops", prefix)
	}
	if opts.PrintContract {
		if opts.Target != "" || opts.ExpectSHA != "" {
			return cmderr.Usagef("--print-contract derives and prints the contract without a target; drop --target and --expect-sha")
		}
		return nil
	}
	switch {
	case opts.Target == "":
		return cmderr.Usagef("qualify needs --target: `local` or the http(s) URL of a running deployment (or --print-contract)")
	case opts.Target == TargetLocal && opts.ExpectSHA != "":
		return cmderr.Usagef("--expect-sha binds a URL target to a deployed build; a local target binds its verdict to the worktree fingerprint instead, so drop --expect-sha")
	case opts.Target == TargetLocal:
		return nil
	}
	if _, err := qualify.ParseTargetURL(opts.Target); err != nil {
		return cmderr.Usagef("%v", err)
	}
	if opts.ExpectSHA == "" {
		return cmderr.Usagef("--expect-sha is required with a URL target: a verdict must name the build it proves")
	}
	if err := qualify.ValidateExpectSHA(opts.ExpectSHA); err != nil {
		return cmderr.Usagef("%v", err)
	}
	return nil
}

func printJSON(document any) error {
	data, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode qualify output: %w", err)
	}
	iox.Fprintln(iox.Stdout(), string(data))
	return nil
}
