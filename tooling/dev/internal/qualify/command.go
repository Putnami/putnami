package qualify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	qualifyproto "go.putnami.dev/protocol/qualify"
	"go.putnami.dev/sdk/extension/resultv2"

	"go.putnami.dev/tooling/dev/internal/cliargs"
	"go.putnami.dev/tooling/dev/internal/compose"
)

// CommandPath is the `command` of every result envelope `putnami qualify`
// writes. The group's default subcommand answers it, so a consumer bound to
// the verdict of `putnami qualify` reads the same document.
const CommandPath = "qualify"

// TargetLocal is the --target value that composes the workload on this machine.
const TargetLocal = "local"

// commandFlags are the flags `putnami qualify` accepts.
var commandFlags = []cliargs.Flag{
	{Long: "--target", ValueName: "<local|url>"},
	{Long: "--expect-sha", ValueName: "<sha>"},
	{Long: "--platform-prefix", ValueName: "<path>"},
	{Long: "--ready-timeout", ValueName: "<duration>"},
	{Long: "--request-timeout", ValueName: "<duration>"},
	{Long: "--print-contract"},
}

// Request is one `putnami qualify` invocation.
type Request struct {
	Ctx context.Context
	// Workspace is the membership the selector resolves in, and the
	// workspace a local target composes.
	Workspace *compose.Workspace
	// Args are the invocation's arguments after the subcommand: the project
	// and the command's flags.
	Args []string
	// Output is the output mode putnami resolved for the invocation.
	Output protocolcli.OutputMode
	// Executable is the putnami CLI a local target's members' serve runs
	// start.
	Executable string
	Stdout     io.Writer
	Stderr     io.Writer
}

// invocation is one parsed `putnami qualify` invocation.
type invocation struct {
	// selector names exactly one project: an ID, a name, or a path.
	selector string
	// target is "local" or an http(s) URL; empty only with printContract.
	target string
	// expectSHA is the sha a URL target must report on /version.
	expectSHA string
	// platformPrefix is where /readyz and /version are mounted. Empty reads
	// it from the route inventory (ResolvePlatform).
	platformPrefix string
	// readyTimeout bounds readiness; zero means the default.
	readyTimeout time.Duration
	// requestTimeout bounds each request; zero means the default.
	requestTimeout time.Duration
	// printContract prints the derived contract and runs nothing.
	printContract bool
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

// Run resolves one workload, derives its smoke contract, runs it against a
// target, and turns the verdict into the command's output. It returns the
// exit code.
//
// The exit code follows the verdict alone. passed exits 0; every other state
// exits 1 and still prints the verdict — in a structured output mode it
// travels as the failure envelope's data — so a caller never has to choose
// between the code and the evidence. Invalid invocations exit 2 before
// anything is derived. SIGINT and SIGTERM cancel the run, which still tears a
// local composition down and prints the canceled verdict.
func Run(req Request) int {
	out := output{stdout: req.Stdout, stderr: req.Stderr, mode: req.Output}
	inv, err := parseInvocation(req.Args)
	if err != nil {
		return out.failure(nil, err)
	}
	if err := validate(inv); err != nil {
		return out.failure(nil, err)
	}
	project := resolveProjectSelector(req.Workspace, inv.selector)
	if project == nil {
		return out.failure(nil, protocolcli.NotFoundf("project not found: %s", inv.selector))
	}
	// Without --platform-prefix, the route inventory names where /readyz and
	// /version are mounted, and whether /readyz is served at all: the same file
	// the contract is derived from.
	platform, err := ResolvePlatform(req.Workspace, project, inv.platformPrefix)
	if err != nil {
		var ambiguous *AmbiguousPlatformPrefixError
		if errors.As(err, &ambiguous) {
			err = protocolcli.Classify(err, protocolcli.ErrInvalidConfig)
		}
		return out.failure(nil, err)
	}
	contract, unsupported, err := Derive(req.Workspace, project, platform.Prefix)
	if err != nil {
		var invalid *InvalidInventoryError
		if errors.As(err, &invalid) {
			err = protocolcli.Classify(err, protocolcli.ErrInvalidConfig)
		}
		return out.failure(nil, err)
	}
	if inv.printContract {
		if out.structured() {
			return out.document(contract)
		}
		if err := RenderContractText(out.stdout, contract, unsupported); err != nil {
			return out.failure(nil, err)
		}
		return protocolcli.ExitSuccess
	}

	target, err := newTarget(req, inv, project)
	if err != nil {
		return out.failure(nil, err)
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	signals, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	verdict := Execute(signals, contract, target, Options{
		PlatformPrefix: platform.Prefix,
		ReadinessRoute: platform.ReadinessRoute,
		ReadyTimeout:   inv.readyTimeout,
		RequestTimeout: inv.requestTimeout,
		Unsupported:    unsupported,
	})
	if verdict.State.IsPass() {
		if out.structured() {
			return out.document(verdict)
		}
		if err := RenderVerdictText(out.stdout, verdict); err != nil {
			return out.failure(nil, err)
		}
		return protocolcli.ExitSuccess
	}
	verdictErr := &VerdictError{Verdict: verdict}
	if out.structured() {
		return out.failure(verdict, verdictErr)
	}
	if err := RenderVerdictText(out.stdout, verdict); err != nil {
		return out.failure(nil, err)
	}
	return out.failure(nil, verdictErr)
}

// parseInvocation reads the project and the flags.
func parseInvocation(args []string) (invocation, error) {
	parsed, err := cliargs.Parse(CommandPath, args, commandFlags)
	if err != nil {
		return invocation{}, err
	}
	if len(parsed.Positionals) != 1 {
		return invocation{}, protocolcli.Usagef("qualify takes exactly one project: putnami qualify <project> --target <local|url>")
	}
	readyTimeout, err := parsePositiveDuration("--ready-timeout", parsed.Value("--ready-timeout"))
	if err != nil {
		return invocation{}, err
	}
	requestTimeout, err := parsePositiveDuration("--request-timeout", parsed.Value("--request-timeout"))
	if err != nil {
		return invocation{}, err
	}
	return invocation{
		selector:       parsed.Positionals[0],
		target:         parsed.Value("--target"),
		expectSHA:      parsed.Value("--expect-sha"),
		platformPrefix: parsed.Value("--platform-prefix"),
		readyTimeout:   readyTimeout,
		requestTimeout: requestTimeout,
		printContract:  parsed.Has("--print-contract"),
	}, nil
}

// parsePositiveDuration reads a Go duration flag; empty means the default (0).
func parsePositiveDuration(flag, raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, protocolcli.Usagef("%s must be a positive Go duration such as 30s or 2m", flag)
	}
	return value, nil
}

// resolveProjectSelector resolves a project id, then a name, then a path
// relative to the workspace root.
func resolveProjectSelector(ws *compose.Workspace, selector string) *compose.Project {
	if strings.HasPrefix(selector, "/") {
		if project := ws.ProjectByID(selector); project != nil {
			return project
		}
	}
	if project := ws.ProjectByName(selector); project != nil {
		return project
	}
	return ws.ProjectByID("/" + strings.Trim(selector, "/"))
}

// newTarget builds the target --target names. A local target fingerprints the
// worktree here, before anything is prepared or started. Its members' serve
// logs are dropped: a failed start's verdict names the member, the phase and
// the cause.
func newTarget(req Request, inv invocation, project *compose.Project) (Target, error) {
	if inv.target != TargetLocal {
		target, err := NewURLTarget(inv.target, inv.expectSHA)
		if err != nil {
			return nil, protocolcli.Usagef("%v", err)
		}
		return target, nil
	}
	return NewLocalTarget(LocalOptions{
		Workspace:    req.Workspace,
		Project:      project,
		ReadyTimeout: inv.readyTimeout,
		Executable:   req.Executable,
	})
}

// validate refuses an invocation that cannot run before any file is read.
func validate(inv invocation) error {
	prefix := inv.platformPrefix
	if strings.ContainsAny(prefix, "?#% \t") || strings.Contains(prefix, "://") {
		return protocolcli.Usagef("--platform-prefix %q must be a plain path such as /_ops", prefix)
	}
	if inv.printContract {
		if inv.target != "" || inv.expectSHA != "" {
			return protocolcli.Usagef("--print-contract derives and prints the contract without a target; drop --target and --expect-sha")
		}
		return nil
	}
	switch {
	case inv.target == "":
		return protocolcli.Usagef("qualify needs --target: `local` or the http(s) URL of a running deployment (or --print-contract)")
	case inv.target == TargetLocal && inv.expectSHA != "":
		return protocolcli.Usagef("--expect-sha binds a URL target to a deployed build; a local target binds its verdict to the worktree fingerprint instead, so drop --expect-sha")
	case inv.target == TargetLocal:
		return nil
	}
	if _, err := ParseTargetURL(inv.target); err != nil {
		return protocolcli.Usagef("%v", err)
	}
	if inv.expectSHA == "" {
		return protocolcli.Usagef("--expect-sha is required with a URL target: a verdict must name the build it proves")
	}
	if err := ValidateExpectSHA(inv.expectSHA); err != nil {
		return protocolcli.Usagef("%v", err)
	}
	return nil
}

// output writes the command's documents in the resolved output mode.
type output struct {
	stdout, stderr io.Writer
	mode           protocolcli.OutputMode
}

// structured reports whether the mode writes documents rather than text.
func (o output) structured() bool {
	return o.mode.IsStructured() || o.mode == protocolcli.OutputCloudLogging
}

// document writes a successful run's document. In json and jsonl it is the
// data of a success envelope, as a JSON value whose object members are in
// key order; in cloud-logging it is the document alone, on one line.
func (o output) document(document any) int {
	data, err := json.Marshal(document)
	if err != nil {
		return o.failure(nil, fmt.Errorf("encode qualify output: %w", err))
	}
	if !o.mode.IsStructured() {
		if _, err := fmt.Fprintln(o.stdout, string(data)); err != nil {
			return o.failure(nil, err)
		}
		return protocolcli.ExitSuccess
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return o.failure(nil, fmt.Errorf("encode qualify output: %w", err))
	}
	return resultv2.Emit(o.stdout, o.stderr, o.mode, CommandPath, "", value, nil)
}

// failure writes a failed run: in json and jsonl the failure envelope, whose
// data is the verdict when there is one, and the error on stderr in every
// mode.
func (o output) failure(data any, err error) int {
	if data != nil && !o.mode.IsStructured() {
		data = nil
	}
	return resultv2.Emit(o.stdout, o.stderr, o.mode, CommandPath, "", data, err)
}
