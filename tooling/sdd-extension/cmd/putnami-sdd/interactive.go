package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/resultv2"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
	"go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The four SDD command groups — `features`, `specs`, `architecture`,
// `contracts` — as this binary answers them.
//
// # Why this path is not cli.Run
//
// Every subcommand of the four groups is `interactive: true` (D6), the one
// deliberate scheduler bypass: the CLI spawns this process with the terminal's
// own stdout and does not parse a byte of it. What is written here IS the
// command's output, so it must be what the CLI would have written for a
// built-in structured command of the same name.
//
// cli.Run cannot do that. It emits the JSONL runtime-event stream — a meta
// event, then a result event — which is exactly right for a DAG task and fatal
// here: a second document beside the result envelope makes `putnami features
// list --output=json | jq` fail. So the interactive subcommands take their own
// entry point, and the jobs keep cli.Run.
//
// # The three things that are easy to get wrong
//
//  1. THE OUTPUT MODE IS A PARAM, NOT A FLAG. `--output` is a reserved global:
//     a manifest may not declare it and the CLI consumes it before dispatch, so
//     the resolved mode arrives as params["output"]
//     (tooling/cli/internal/cli/extension_command.go:144-175). Reading it is
//     this file's job; framing the envelope with it is resultv2.Emit's.
//
//  2. POSITIONALS ARRIVE THROUGH jobDef.Args. The CLI appends the raw job args
//     to the manifest task's own args, so argv is
//     `<task-arg> <user args…> --putnamiContext <file>`. The arity checks and
//     their messages below are VERBATIM from the handlers this replaces
//     (registry_commands.go:200-367); they are user-visible contract.
//
//  3. SELECTION COMES FROM THE WIRE. The CLI resolved `--projects/--impacted/…`
//     before spawning us and published the answer as `selection` (D3). Three
//     raw flags cannot be read back out of that resolved answer — `--projects`'
//     original spelling, `--all`, and a standalone `--baseline` — so the CLI
//     forwards those three as params. Nothing here re-resolves anything.

// subcommand is one entry of the four command groups: what it is called, what
// it accepts, and what it runs.
type subcommand struct {
	// group and name form the envelope's `command` path ("features list").
	group string
	name  string
	// arity validates the positional arguments and returns the VERBATIM usage
	// error the CLI handler returned. nil means "any number", which is what
	// `contracts generate|check` had (they target with --project, not with a
	// positional).
	arity func(args []string) error
	// rejectsSelection marks a subcommand whose target is already exact — one
	// feature id, one domain, two revisions. Narrowing an exact lookup can only
	// turn a found answer into a missing one, so the flags are refused rather
	// than accepted and ignored.
	rejectsSelection bool
	// run is the subcommand body: it calls ONE engine builder and says how the
	// result is shown.
	run func(*interactiveRun) outcome
}

// outcome is what one subcommand produced: the payload, the human rendering,
// and the verdict.
//
// human is nil for the cases where the CLI handler returned its error WITHOUT
// rendering anything — a workspace that would not load, a selector that was
// rejected before any report existed. Those handlers guarded on
// `report.Revision.Kind == ""`: a report with no resolved revision describes
// nothing, and printing its hollow "… failed" block would bury the real error.
// Each body below states its own guard rather than inferring one, because the
// guard is per command in core too.
type outcome struct {
	data  any
	human func(w io.Writer)
	err   error
}

// interactiveRun is one subcommand invocation: the wire it was given, the view
// it is entitled to, and the arguments the user typed.
type interactiveRun struct {
	ctx       *pctx.Context
	ws        *wsview.Workspace
	selection sdd.Selection
	args      []string
	stdout    io.Writer
	stderr    io.Writer
	mode      protocolcli.OutputMode
}

// interactiveSubcommands is the dispatch table, keyed by the command PATH —
// "features list" — which is also the envelope's `command` field.
//
// The manifest task passes the group and the subcommand as its two own args, so
// this binary reads them positionally and everything after them is the user's.
// Spelling the key as the path (rather than as a single hyphenated token) keeps
// it identical to the string a user typed, and keeps `features validate` — the
// interactive subcommand — visibly distinct from `features-validate`, the DAG
// task of the same subject.
func interactiveSubcommands() map[string]subcommand {
	table := []subcommand{
		{
			group: "features", name: "list",
			arity: atMostOne("features list takes at most one [query]"),
			run:   runFeaturesList,
		},
		{
			group: "features", name: "validate",
			arity: noPositionals("features validate takes no positional arguments"),
			run:   runFeaturesValidateCommand,
		},
		{
			group: "features", name: "snapshot",
			arity: noPositionals("features snapshot takes no positional arguments"),
			run:   runFeaturesSnapshot,
		},
		{
			group: "features", name: "inspect",
			arity:            exactly(1, "features inspect requires exactly one <feature-id>"),
			rejectsSelection: true,
			run:              runFeaturesInspect,
		},
		{
			group: "features", name: "diff",
			arity:            exactly(2, "features diff requires exactly <base-revision> and <head-revision>"),
			rejectsSelection: true,
			run:              runFeaturesDiff,
		},
		{
			group: "specs", name: "list",
			arity: noPositionals("specs list takes no positional arguments"),
			run:   runSpecsList,
		},
		{
			group: "specs", name: "validate",
			arity: noPositionals("specs validate takes no positional arguments"),
			run:   runSpecsValidateCommand,
		},
		{
			group: "specs", name: "verify",
			arity: noPositionals("specs verify takes no positional arguments"),
			run:   runSpecsVerify,
		},
		{
			// `specs baseline` derives the whole-workspace enforce floor, so a
			// project selection could only be silently ignored — rejected instead.
			group: "specs", name: "baseline",
			arity:            noPositionals("specs baseline takes no positional arguments"),
			rejectsSelection: true,
			run:              runSpecsBaseline,
		},
		{
			group: "specs", name: "inspect",
			arity:            exactly(1, "specs inspect requires exactly one <feature-id>"),
			rejectsSelection: true,
			run:              runSpecsInspect,
		},
		{
			group: "specs", name: "init",
			arity:            exactly(1, "specs init requires exactly one <feature-id>"),
			rejectsSelection: true,
			run:              runSpecsInit,
		},
		{
			group: "architecture", name: "validate",
			arity: noPositionals("architecture validate takes no positional arguments"),
			run:   runArchitectureValidateCommand,
		},
		{
			group: "architecture", name: "snapshot",
			arity: noPositionals("architecture snapshot takes no positional arguments"),
			run:   runArchitectureSnapshot,
		},
		{
			group: "architecture", name: "inspect",
			arity: exactly(1, "architecture inspect requires exactly one <domain-id>"),
			run:   runArchitectureInspect,
		},
		{
			// `architecture init` is the one subcommand of the four groups whose
			// project selection is CONTENT rather than scope: the resolved
			// selection is the project list the scaffolded domain maps, so the
			// flags are honored instead of refused.
			group: "architecture", name: "init",
			arity: exactly(1, "architecture init requires exactly one <domain-id>"),
			run:   runArchitectureInit,
		},
		{
			// `architecture sync` reconciles every declared manifest against the
			// whole resolved graph, so a project selection could only be silently
			// ignored — rejected instead, exactly as `specs baseline` does.
			group: "architecture", name: "sync",
			arity:            noPositionals("architecture sync takes no positional arguments"),
			rejectsSelection: true,
			run:              runArchitectureSync,
		},
		// `contracts` declares no positional arity: both subcommands name their
		// project with `--project <selector>`, and core checked nothing here.
		{group: "contracts", name: "generate", run: runContractsGenerate},
		{group: "contracts", name: "check", run: runContractsCheck},
	}
	byPath := make(map[string]subcommand, len(table))
	for _, entry := range table {
		byPath[entry.group+" "+entry.name] = entry
	}
	return byPath
}

// lookupInteractiveSubcommand resolves the leading `<group> <subcommand>` pair
// of an argument vector, returning the entry and the arguments that follow it.
func lookupInteractiveSubcommand(args []string) (subcommand, []string, bool) {
	if len(args) < 2 {
		return subcommand{}, nil, false
	}
	entry, found := interactiveSubcommands()[args[0]+" "+args[1]]
	if !found {
		return subcommand{}, nil, false
	}
	return entry, args[2:], true
}

// runInteractiveSubcommand is the whole interactive entry point: parse the
// reserved arguments, build the view, dispatch, frame the answer. It returns
// the process exit code.
func runInteractiveSubcommand(sub subcommand, args []string, stdout, stderr io.Writer) int {
	contextFile, positionals, err := splitReservedArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "putnami-sdd: %v\n", err)
		return protocolcli.ExitUsage
	}
	ctx, err := pctx.Parse(contextFile)
	if err != nil {
		fmt.Fprintf(stderr, "putnami-sdd: %v\n", err)
		return protocolcli.ExitUsage
	}
	return dispatchInteractive(sub, ctx, positionals, stdout, stderr)
}

// dispatchInteractive is the entry point minus the two process-level concerns
// above — reading argv and reading the context file — so a test drives the real
// dispatch with a context value instead of a temporary file.
func dispatchInteractive(sub subcommand, ctx *pctx.Context, positionals []string, stdout, stderr io.Writer) int {
	run := &interactiveRun{
		ctx:    ctx,
		args:   positionals,
		stdout: stdout,
		stderr: stderr,
		mode:   resolvedOutputMode(ctx),
	}
	run.ws, _ = wsview.FromContext(ctx)
	run.selection = wireSelection(ctx, run.ws)

	if sub.arity != nil {
		if err := sub.arity(positionals); err != nil {
			return run.emit(sub, outcome{err: err})
		}
	}
	if sub.rejectsSelection {
		if err := run.rejectProjectSelection(sub.group + " " + sub.name); err != nil {
			return run.emit(sub, outcome{err: err})
		}
	}
	return run.emit(sub, sub.run(run))
}

// emit writes one subcommand's answer and returns the exit code.
//
// The two halves are the CLI's own (App.runStructuredCommand): in a structured
// mode nothing but the envelope reaches stdout, and in a human mode the
// renderer owns stdout while the error line goes to stderr. The FAILURE
// envelope's payload is the data attached to the error — never the report the
// body happened to build — because that is what writeStructuredFailure reads
// (shared.ResultData) and a body that failed early has none.
func (run *interactiveRun) emit(sub subcommand, result outcome) int {
	if !run.mode.IsStructured() && result.human != nil {
		result.human(run.stdout)
	}
	data := result.data
	if result.err != nil {
		data = sdd.ResultData(result.err)
	} else {
		data = capturedPayload(data)
	}
	return resultv2.Emit(run.stdout, run.stderr, run.mode, sub.group, sub.name, data, result.err)
}

// capturedPayload reproduces the round trip a SUCCESSFUL built-in structured
// command's payload takes, and it exists for one reason: JSON key order.
//
// App.runStructuredCommand does not hand the handler's report to the encoder.
// It CAPTURES the handler's stdout, decodes it with capturedResultEnvelope into
// a protocolcli.ResultV2 whose Data is `any` — so every object becomes a
// map[string]any — and re-encodes that (commands.go:96-97). Go sorts map keys,
// so a built-in command's `data` object comes out ALPHABETICAL while a Go
// struct encodes in field-declaration order. Emitting the struct directly is
// therefore byte-different from every command this extension replaces, in a way
// no consumer asked for.
//
// A FAILURE takes the other path and must not be round-tripped:
// writeStructuredFailure passes shared.ResultData(err) — the attached value
// itself — straight to NewResultV2 (commands.go:186), so a failure payload keeps
// struct order. That asymmetry is core's, not a choice made here, and
// sdd_extraction_parity_test.go in tooling/cli pins both halves against the
// live built-in.
//
// A payload that cannot round-trip is returned unchanged rather than dropped: an
// encoding failure here would turn a correct answer into no answer.
func capturedPayload(data any) any {
	if data == nil {
		return nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return data
	}
	var decoded any
	if json.Unmarshal(encoded, &decoded) != nil {
		return data
	}
	return decoded
}

// rejectProjectSelection refuses project selection on a subcommand whose target
// is already exact. Message and behavior are verbatim from
// `rejectProjectSelection` in tooling/cli/internal/cli/registry_commands.go.
func (run *interactiveRun) rejectProjectSelection(path string) error {
	if !run.anySelectionFlag() {
		return nil
	}
	return protocolcli.Usagef("%s takes an exact target, so project selection does not apply: "+
		"drop --projects/--impacted/--all/--baseline/--tag/--exclude-tag/--exclude "+
		"(use `putnami features list` or `putnami specs list` to narrow by project)", path)
}

// anySelectionFlag reports what `shared.ProjectSelection.Any()` reports, read
// off the wire instead of off the parsed flags.
//
// Two of its three terms are visible in the RESOLVED selection: `--projects`,
// `--impacted`, `--tag`, `--exclude-tag` and `--exclude` all make the run
// scoped, and `scoped` is on the wire. The other two are not, and cannot be:
// `--all` resolves to the same answer as passing nothing (it is the explicit
// spelling of the default), and a standalone `--baseline` resolves to nothing
// at all because only `--impacted` consumes it. The CLI therefore forwards
// those two as params on the interactive path.
func (run *interactiveRun) anySelectionFlag() bool {
	if run.ctx == nil {
		return false
	}
	if run.ctx.Selection != nil && run.ctx.Selection.Scoped {
		return true
	}
	return run.ctx.Params.Bool(paramAll, false) || strings.TrimSpace(run.baseline()) != ""
}

// baseline is the raw global `--baseline` value: the immutable Git comparison
// point `architecture` compares against, and one of the flags the exact-target
// subcommands refuse.
func (run *interactiveRun) baseline() string {
	if run.ctx == nil {
		return ""
	}
	return run.ctx.Params.String(paramBaseline)
}

// Params the CLI forwards for the interactive path because the resolved
// `selection` block cannot round-trip them.
const (
	paramOutput   = "output"
	paramAll      = "all"
	paramBaseline = "baseline"
	paramProjects = "projects"
	paramDryRun   = "dry-run"
	paramSession  = "session"
	paramUpdate   = "update"
	paramApply    = "apply"
	paramOwner    = "owner"
	paramAt       = "at"
)

// param reads one forwarded string parameter, trimmed. An absent parameter and
// an empty one are the same thing here: both mean "the caller did not say".
func (run *interactiveRun) param(name string) string {
	if run.ctx == nil {
		return ""
	}
	return strings.TrimSpace(run.ctx.Params.String(name))
}

// resolvedOutputMode reads the mode the CLI resolved for this invocation.
//
// An absent or unrecognized value is OutputAuto — human rendering — which is
// also what the CLI forwards nothing for: it declines to send OutputAuto so an
// extension's own default is never overridden.
func resolvedOutputMode(ctx *pctx.Context) protocolcli.OutputMode {
	if ctx == nil {
		return protocolcli.OutputAuto
	}
	mode := protocolcli.OutputMode(ctx.Params.String(paramOutput))
	if !mode.Valid() {
		return protocolcli.OutputAuto
	}
	return mode
}

// wireSelection projects the context's resolved selection onto the value the
// engines take.
//
// A context with no `selection` block comes from an orchestrator older than
// D3. The fallback is the unscoped whole-workspace projection — mode "all",
// every member, scoped false — which is exactly what the CLI resolves for an
// invocation with no selection flags, so an old orchestrator degrades to the
// default answer rather than to an empty one.
func wireSelection(ctx *pctx.Context, ws *wsview.Workspace) sdd.Selection {
	if ctx != nil && ctx.Selection != nil {
		return *ctx.Selection
	}
	ids := []string{}
	if ws != nil {
		for _, project := range ws.Projects {
			if project != nil {
				ids = append(ids, project.ID)
			}
		}
	}
	return sdd.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: ids}
}

// splitReservedArgs pulls the reserved arguments out of the argument vector and
// returns the positionals the user typed.
//
// `--putnamiContext` is the orchestrator's, appended after the user's tokens.
// `--output`/`--json` are consumed by the CLI before dispatch and can therefore
// never appear here; they are stripped anyway, and identically to the SDK's own
// parser, so a direct invocation of this binary behaves like a job's.
func splitReservedArgs(args []string) (contextFile string, positionals []string, err error) {
	positionals = make([]string, 0, len(args))
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		switch {
		case arg == "--putnamiContext":
			if len(args) == 0 {
				return "", nil, protocolcli.Usagef("--putnamiContext requires a value")
			}
			contextFile, args = args[0], args[1:]
		case strings.HasPrefix(arg, "--putnamiContext="):
			contextFile = strings.TrimPrefix(arg, "--putnamiContext=")
		case arg == "--output":
			if len(args) > 0 {
				args = args[1:]
			}
		case strings.HasPrefix(arg, "--output="), arg == "--json":
			// Consumed: reserved, and resolved into params["output"] instead.
		default:
			positionals = append(positionals, arg)
		}
	}
	if contextFile == "" {
		return "", nil, protocolcli.Usagef("--putnamiContext is required")
	}
	return contextFile, positionals, nil
}

// subcommandValueFlags is every subcommand-local flag that takes a value, by the
// name the CLI forwards it under.
//
// It has to be a set rather than one special case. The CLI rewrites `--owner=x`
// into two tokens before forwarding (splitInlineValues), so both spellings reach
// an extension as `--owner` `x`; a flag missing from this set therefore donates
// its value to the positional count and the subcommand rejects a correct call.
// TestEveryValueTakingSubcommandFlagIsKnown holds the set to the committed
// extension manifest, so a new string flag cannot be added without it.
var subcommandValueFlags = map[string]bool{
	"--at":      true,
	"--owner":   true,
	"--project": true,
	"--session": true,
}

// positionalArgs returns the tokens that are not flags. The CLI hands an
// extension every token it did not consume, so a subcommand-local flag
// (`contracts --project <id>`, `architecture init --owner <team>`) travels beside
// the positionals and must not be counted as one.
func positionalArgs(args []string) []string {
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}
		name, _, inline := strings.Cut(arg, "=")
		// An inline `--owner=x` already carries its value, so only the separate
		// spelling consumes the next token.
		if !inline && subcommandValueFlags[name] && index+1 < len(args) {
			index++
		}
	}
	return positionals
}

func noPositionals(message string) func([]string) error {
	return func(args []string) error {
		if len(positionalArgs(args)) != 0 {
			return protocolcli.Usagef("%s", message)
		}
		return nil
	}
}

func atMostOne(message string) func([]string) error {
	return func(args []string) error {
		if len(positionalArgs(args)) > 1 {
			return protocolcli.Usagef("%s", message)
		}
		return nil
	}
}

func exactly(count int, message string) func([]string) error {
	return func(args []string) error {
		if len(positionalArgs(args)) != count {
			return protocolcli.Usagef("%s", message)
		}
		return nil
	}
}

// --- features ---------------------------------------------------------------

func runFeaturesList(run *interactiveRun) outcome {
	query := ""
	if positionals := positionalArgs(run.args); len(positionals) == 1 {
		query = positionals[0]
	}
	report, err := sdd.BuildFeatureCatalogResult(run.ws, query, run.selection)
	if err != nil {
		// Discovery degrades rather than fails, so the only error here is a
		// rejected query or an absent workspace — neither of which produced a
		// report to show.
		return outcome{err: err}
	}
	return outcome{data: report, human: func(w io.Writer) { printFeatureCatalogHuman(w, report) }}
}

func runFeaturesValidateCommand(run *interactiveRun) outcome {
	report, verdict := sdd.BuildFeatureValidationResult(run.ws, run.selection)
	if verdict != nil && sdd.ResultData(verdict) == nil {
		// The evaluation could not start (no workspace on the wire). Core
		// returned that error without rendering.
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printFeatureValidationHuman(w, report) },
		err:   verdict,
	}
}

func runFeaturesSnapshot(run *interactiveRun) outcome {
	report, verdict := sdd.BuildFeatureSnapshotResult(run.ws, run.selection)
	if verdict != nil && sdd.ResultData(verdict) == nil {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printFeatureSnapshotHuman(w, report) },
		err:   verdict,
	}
}

func runFeaturesInspect(run *interactiveRun) outcome {
	report, verdict := sdd.BuildFeatureContextResult(run.ws, positionalArgs(run.args)[0])
	if verdict != nil && report.Revision.Kind == "" {
		// The builder could not get far enough to describe anything. Every
		// rendered report carries a resolved revision, so an empty one means
		// there is nothing to print.
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printFeatureInspectionHuman(w, report) },
		err:   verdict,
	}
}

func runFeaturesDiff(run *interactiveRun) outcome {
	positionals := positionalArgs(run.args)
	report, verdict := sdd.BuildFeatureDiffResult(run.workspaceRoot(), positionals[0], positionals[1])
	return outcome{
		data:  report,
		human: func(w io.Writer) { printFeatureDiffHuman(w, report, verdict) },
		err:   verdict,
	}
}

// --- specs ------------------------------------------------------------------

func runSpecsList(run *interactiveRun) outcome {
	report, verdict := sdd.BuildSpecCatalogResult(run.ws, run.selection)
	if verdict != nil && report.Revision.Kind == "" {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printSpecCatalogHuman(w, report) },
		err:   verdict,
	}
}

func runSpecsValidateCommand(run *interactiveRun) outcome {
	report, verdict := sdd.BuildSpecValidationResult(run.ws, run.selection)
	if verdict != nil && report.Revision.Kind == "" {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printSpecValidationHuman(w, report) },
		err:   verdict,
	}
}

// runSpecsVerify replays the recorded spec-gate verdict for the selection —
// the audit surface of the executable-spec gate. `--session` names an exact recorded
// session; the default is the latest one. The blocking decision it reports is
// the one core persisted (or, for specs no session covered, the same shared
// blocking rule over zero observations), never a second opinion.
func runSpecsVerify(run *interactiveRun) outcome {
	session := ""
	if run.ctx != nil {
		session = run.ctx.Params.String(paramSession)
	}
	report, verdict := sdd.BuildSpecVerifyResult(run.ws, run.selection, session)
	if verdict != nil && report.Revision.Kind == "" {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printSpecVerifyHuman(w, report) },
		err:   verdict,
	}
}

// runSpecsBaseline reports the enforced-spec floor the committed worktree
// states, compared byte-for-byte with the committed specs.baseline.json
// files, one per enforced project; `--update` rewrites them to the canonical
// derived floor and deletes a file the floor no longer names. This is the one
// place the ratchet's baseline is raised (or deliberately lowered) — the
// validate-workspace task only ever compares.
func runSpecsBaseline(run *interactiveRun) outcome {
	update := run.ctx != nil && run.ctx.Params.Bool(paramUpdate, false)
	report, verdict := sdd.BuildSpecsBaselineResult(run.ws, update)
	if verdict != nil && report.Revision.Kind == "" {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printSpecsBaselineHuman(w, report) },
		err:   verdict,
	}
}

func runSpecsInspect(run *interactiveRun) outcome {
	report, verdict := sdd.BuildSpecContextResult(run.ws, positionalArgs(run.args)[0])
	if verdict != nil && report.Revision.Kind == "" {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printSpecContextHuman(w, report) },
		err:   verdict,
	}
}

// runSpecsInit is the one non-read-only subcommand of the four groups. It takes
// the global `--dry-run` rather than a flag of its own, exactly as `projects
// sync` does, which is why the manifest declares `dry-run` on it: the CLI
// forwards the global only to a subcommand whose effective flag surface names
// it (extension_command.go:180-186).
func runSpecsInit(run *interactiveRun) outcome {
	dryRun := run.ctx != nil && run.ctx.Params.Bool(paramDryRun, false)
	report, verdict := sdd.BuildSpecInitResult(run.ws, positionalArgs(run.args)[0], dryRun)
	if verdict != nil && report.Revision.Kind == "" {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printSpecInitHuman(w, report) },
		err:   verdict,
	}
}

// --- architecture -----------------------------------------------------------

func runArchitectureValidateCommand(run *interactiveRun) outcome {
	report, verdict := sdd.BuildArchitectureValidationResult(run.ws, run.baseline())
	if verdict != nil && sdd.ResultData(verdict) == nil {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printArchitectureValidationHuman(w, report) },
		err:   verdict,
	}
}

func runArchitectureSnapshot(run *interactiveRun) outcome {
	report, verdict := sdd.BuildArchitectureSnapshotResult(run.ws, run.baseline())
	if verdict != nil && sdd.ResultData(verdict) == nil {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printArchitectureSnapshotHuman(w, report) },
		err:   verdict,
	}
}

func runArchitectureInspect(run *interactiveRun) outcome {
	report, verdict := sdd.BuildArchitectureInspectionResult(run.ws, positionalArgs(run.args)[0], run.baseline())
	if verdict != nil && sdd.ResultData(verdict) == nil {
		return outcome{err: verdict}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printArchitectureInspectionHuman(w, report) },
		err:   verdict,
	}
}

// runArchitectureInit scaffolds one domain manifest. The domain ID is the
// positional; the project list is the RESOLVED selection, which is why this
// subcommand honors the selection flags the other exact-target ones refuse.
//
// Two selection modes are refused rather than obeyed. `--impacted` resolves to a
// diff, and a diff is not a membership statement. `--all` would claim every
// project in the workspace for one domain, which is never what an author means
// and is expensive to undo once a permission list is built on it.
func runArchitectureInit(run *interactiveRun) outcome {
	if err := run.architectureInitSelection(); err != nil {
		return outcome{err: err}
	}
	options := sdd.ArchitectureInitOptions{
		Owner:    run.param(paramOwner),
		At:       run.param(paramAt),
		DryRun:   run.ctx != nil && run.ctx.Params.Bool(paramDryRun, false),
		Projects: run.selection.ProjectIDs,
	}
	if !run.selection.Scoped {
		// An unscoped run maps no project: `"projects": []` is the protocol's own
		// way of saying "this domain has no members yet", and it is a far better
		// starting point than the whole workspace.
		options.Projects = nil
	}
	report, verdict := sdd.BuildArchitectureInitResult(run.ws, positionalArgs(run.args)[0], options)
	return outcome{
		data:  report,
		human: func(w io.Writer) { printArchitectureInitHuman(w, report) },
		err:   verdict,
	}
}

// architectureInitSelection refuses the two selection modes that cannot mean a
// domain's membership.
func (run *interactiveRun) architectureInitSelection() error {
	if run.ctx != nil && run.ctx.Params.Bool(paramAll, false) {
		return protocolcli.Usagef("architecture init maps the projects you name, so --all does not apply: " +
			"pass --projects or --scope, or none to scaffold a domain that maps no project yet")
	}
	if run.selection.Mode == pctx.SelectionModeImpacted {
		return protocolcli.Usagef("architecture init maps the projects you name, so --impacted does not apply: " +
			"an impacted set is what changed, not what a domain contains")
	}
	return nil
}

// runArchitectureSync reconciles the mechanical half of every declared manifest
// with the resolved graph. Without --apply it prints the suggestion and writes
// nothing; the git diff of an applied run is the authorization moment for every
// binding it proposes.
func runArchitectureSync(run *interactiveRun) outcome {
	apply := run.ctx != nil && run.ctx.Params.Bool(paramApply, false)
	report, verdict := sdd.BuildArchitectureSyncResult(run.ws, apply)
	return outcome{
		data:  report,
		human: func(w io.Writer) { printArchitectureSyncHuman(w, report) },
		err:   verdict,
	}
}

// --- contracts --------------------------------------------------------------

func runContractsGenerate(run *interactiveRun) outcome {
	project, err := run.contractsProject()
	if err != nil {
		return outcome{err: err}
	}
	report, err := sdd.ContractsGenerate(filepath.Join(run.workspaceRoot(), project.Path), project.ID)
	if err != nil {
		return outcome{err: err}
	}
	return outcome{data: report, human: func(w io.Writer) { printContractsGenerateHuman(w, report) }}
}

// runContractsCheck preserves the exit-code contract the compiler's review cycle
// depends on: drift or a breaking change is an ErrInvalidConfig-classified
// failure, which the shared taxonomy maps to exit 2. It stays interactive-only
// in v1 (D2) for that reason among others — a DAG task has no room for a
// verdict whose exit code is its message.
func runContractsCheck(run *interactiveRun) outcome {
	project, err := run.contractsProject()
	if err != nil {
		return outcome{err: err}
	}
	projectDir := filepath.Join(run.workspaceRoot(), project.Path)
	report, checkErr := sdd.ContractsCheck(projectDir, project.ID, sdd.GitPriorManifest(run.workspaceRoot(), project.Path))
	if report.Outcome == "" {
		// An early error (missing or invalid manifest) built no report.
		return outcome{err: checkErr}
	}
	return outcome{
		data:  report,
		human: func(w io.Writer) { printContractsCheckHuman(w, report) },
		err:   checkErr,
	}
}

// contractsProject resolves the single project `contracts generate|check`
// operates on, from the subcommand's own `--project` flag and then from the
// global `--projects` selector — the same order and the same refusals as
// `resolveContractsProject` in core.
//
// The global selector is read RAW, as the CLI parsed it, rather than from the
// resolved `selection` block: the parser writes its own sentinels into that
// field (`--all` becomes "*", `--impacted` becomes "[impacted]") and both are
// refused here by name. Resolving first and reading the ids back would accept
// a `--projects @scope/*` that core refuses, which is a wider surface, not a
// compatible one.
func (run *interactiveRun) contractsProject() (*wsview.Project, error) {
	selector, err := parseProjectFlag(run.args)
	if err != nil {
		return nil, err
	}
	if selector == "" && run.ctx != nil {
		selector = strings.TrimSpace(run.ctx.Params.String(paramProjects))
	}
	if selector == "" || selector == "*" || selector == "[impacted]" {
		return nil, protocolcli.Classify(
			errors.New("contracts requires a single target project: pass --project <id|name|path>"),
			protocolcli.ErrUsage)
	}
	if run.ws == nil {
		return nil, protocolcli.InvalidConfigf("the job context carries no workspace")
	}
	project := sdd.ResolveProjectSelector(run.ws, selector)
	if project == nil {
		return nil, protocolcli.NotFoundf("project not found: %s", selector)
	}
	return project, nil
}

// parseProjectFlag reads `--project <value>` / `--project=<value>` out of the
// argument list. Copied from `shared.ParseProjectFlag`, whose message is the one
// a user of `contracts generate` reads today.
func parseProjectFlag(args []string) (string, error) {
	missing := func() (string, error) {
		return "", protocolcli.Classify(
			errors.New("--project requires a value: pass --project <id|name|path>"),
			protocolcli.ErrUsage)
	}
	for index := range len(args) {
		arg := args[index]
		if arg == "--project" {
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				return args[index+1], nil
			}
			return missing()
		}
		if value, found := strings.CutPrefix(arg, "--project="); found {
			if strings.TrimSpace(value) == "" {
				return missing()
			}
			return value, nil
		}
	}
	return "", nil
}

// workspaceRoot is the absolute workspace root the orchestrator resolved. The
// two subcommands that need it — `features diff` over git history and
// `contracts` over a project directory — take it from the wire and never from
// the process's working directory.
func (run *interactiveRun) workspaceRoot() string {
	if run.ctx == nil {
		return ""
	}
	return run.ctx.WorkspaceRoot
}
