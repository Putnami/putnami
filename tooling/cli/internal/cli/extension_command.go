package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// runExtensionStructuredCommand dispatches a structured command exposed by an
// extension manifest's commandGroups block (e.g. "putnami cloud login").
//
// Subcommands marked interactive bypass the live job renderer and inherit the
// terminal's stdin/stdout/stderr (e.g. cloud login). Non-interactive subcommands
// run through the standard job pipeline so progress events, caching, and renderer
// chrome behave consistently with flat job commands.
func (a *App) runExtensionStructuredCommand(
	ctx context.Context,
	parsed *ParsedArgs,
	cfg *wsproto.Config,
	wsRoot string,
	extensions []*extension.ExtensionDescription,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	group := parsed.Commands[0]
	sub := parsed.Subcommand

	// A collaboration contract word is routed to its bound provider, never to
	// an extension command group of the same name.
	if isCollaborationCommand(wsRoot, group) {
		return a.runCollaborationCommand(ctx, parsed, cfg, wsRoot, extensions, stdin, stdout, stderr)
	}

	// `putnami <group>` with no subcommand word runs the group's declared
	// default, exactly like a built-in DefaultSub; flags that follow reach it.
	// A group without one, or whose default does not resolve, prints its help.
	// `putnami <group> --help` never gets here: App.Run prints the group help.
	if sub == "" {
		sub = extension.GroupDefaultSubcommand(extensions, group)
		if sub == "" {
			printExtensionGroupHelp(extensions, group, parsed.Global.Output)
			return ExitSuccess
		}
	}

	// `putnami <group> help [<sub>]` is a help alias, unless the group defines a
	// real subcommand literally named "help" (guard against shadowing it).
	if sub == "help" {
		if _, _, ok := extension.LookupSubcommand(extensions, group, "help"); !ok {
			if len(parsed.RawJobArgs) > 0 && !strings.HasPrefix(parsed.RawJobArgs[0], "-") {
				printExtensionSubcommandHelp(extensions, group, parsed.RawJobArgs[0], parsed.Global.Output)
			} else {
				printExtensionGroupHelp(extensions, group, parsed.Global.Output)
			}
			return ExitSuccess
		}
	}

	if wsRoot == "" {
		return a.runUserScopeSubcommand(ctx, parsed, cfg, extensions, group, sub, stdin, stdout, stderr)
	}

	resolved, owner := extension.ResolveSubcommand(extensions, group, sub)
	if resolved == nil {
		if owner == nil {
			iox.Fprintf(stderr, "putnami: unknown command: %s\n", group)
			return ExitError
		}
		iox.Fprintf(stderr, "putnami: unknown subcommand: %s %s\n", group, sub)
		if subs := extension.ListSubcommands(extensions, group); len(subs) > 0 {
			iox.Fprintf(stderr, "  Available: %s\n", strings.Join(subs, ", "))
		}
		return ExitError
	}
	// Extensions may contribute different subcommands to the same group. Only
	// duplicate executable definitions for the requested subcommand leave the
	// dispatcher with a discovery-order-dependent choice.
	if owners := extension.CommandSubcommandOwners(extensions, group, sub); len(owners) > 1 {
		iox.Fprintf(stderr, "putnami: command %q is claimed by %s\n", group+" "+sub, strings.Join(owners, " and "))
		iox.Fprintf(stderr, "  Remove one of them, or ask their authors to rename the command.\n")
		return ExitUsage
	}
	if err := jobs.ValidateReleaseSetProviderResolution(
		ctx,
		resolved.Extension.Name,
		resolved.Extension.Version,
		resolved.CommandName,
		resolved.Subdef.Interactive,
	); err != nil {
		iox.Fprintf(stderr, "putnami: %v\n", err)
		return ExitUsage
	}
	if err := resolveCacheTrust(&parsed.Global, cfg, []string{resolved.CommandName}, ""); err != nil {
		iox.Fprintf(stderr, "putnami: %v\n", err)
		return ExitUsage
	}
	if err := resolveCPUPolicy(&parsed.Global, cfg, []string{resolved.CommandName}); err != nil {
		iox.Fprintf(stderr, "putnami: %v\n", err)
		return ExitUsage
	}

	engine.ApplyEnvOverrides(&parsed.Global, cfg)

	// A subcommand inherits the group's shared flags and layers its own on top of
	// the flat command target. Resolve that surface once so their defaults reach
	// params exactly like the flat command's own flags do — in the dry-run
	// forwarding below, in the interactive job context, and (via the planned
	// alias) in plan-time param resolution.
	effectiveFlags := resolved.EffectiveFlags()

	// An extension group's flag surface comes from a manifest on its own release
	// cadence, so an undeclared flag is a deprecation warning for one minor
	// version rather than a hard rejection. The tokens
	// themselves are untouched: they still reach commandParams exactly as before.
	printParseWarnings(stderr, parsed.Global,
		undeclaredFlagWarnings(group+" "+sub, parsed.RawJobArgs, effectiveFlags))

	commandParams := buildExtensionCommandParams(parsed, effectiveFlags)

	if resolved.Subdef.Interactive {
		// The interactive path is the ONE deliberate scheduler bypass: the
		// subprocess inherits the terminal's stdin/stdout, which no renderer-driven
		// run can offer. It therefore loads the workspace and stamps the version
		// here; every other alias hands both to Engine.Run below.
		ws, err := workspace.Load(wsRoot)
		if err != nil {
			iox.Fprintf(stderr, "putnami: load workspace: %v\n", err)
			return ExitError
		}
		// And the runtime it is about to exec, for the same reason: the
		// scheduler bypass skips Engine.Run, which is where
		// SynchronizeExtensionRuntimesForCommands would otherwise have prepared
		// it (engine.go). Without this, a local extension whose runtime has never
		// been built answers every interactive subcommand with
		// `runtime.executable_missing` until an unrelated command — install,
		// projects sync, a planned job — happens to prepare it.
		//
		// This is the same repair internal/mcp made for extension-contributed
		// tools (extension_tool_workspace.go), and it is written the same way: a
		// no-op when the executable is already resolved, so a caller that
		// supplied one is untouched, and silent when the extension declares no
		// runtime at all, because that case has its own named failure downstream.
		if resolved.Extension != nil && resolved.Extension.Runtime != nil &&
			resolved.Extension.RuntimeExecutable == "" {
			if err := jobs.SynchronizeExtensionRuntimes(
				ctx, ws, []*extension.ExtensionDescription{resolved.Extension}, nil); err != nil {
				iox.Fprintf(stderr, "putnami: prepare the %s runtime: %v\n", resolved.Extension.Name, err)
				return ExitError
			}
		}
		// The selection flags the user typed, resolved here because this path
		// plans nothing and would otherwise hand the subprocess a context with no
		// answer at all — the state that made `--projects` on an interactive
		// extension subcommand a flag the CLI parsed and dropped.
		selection, err := shared.ResolveProjectSelection(ws, shared.ProjectSelection{
			Projects:   parsed.Global.Projects,
			All:        parsed.Global.All,
			Impacted:   parsed.Global.Impacted,
			Baseline:   parsed.Global.Baseline,
			FilterTag:  parsed.Global.FilterTag,
			ExcludeTag: parsed.Global.ExcludeTag,
			Exclude:    parsed.Global.Exclude,
		})
		if err != nil {
			// A selection that cannot be resolved FAILS the command rather than
			// running it unscoped. Dropping the member instead would hand the
			// extension a whole-workspace run for a selector that named nothing,
			// which is the wrong answer to the question the user asked — and
			// silently so.
			iox.Fprintf(stderr, "putnami: %v\n", err)
			return exitCodeForError(err)
		}
		wireSelection := selection.Wire()
		jobDef := *resolved.JobDefinition
		// EffectiveFlags is a fresh superset of the command's own flags, so
		// pointing the copy at it never drops a command flag and never mutates
		// the shared extension's flag map.
		jobDef.Flags = effectiveFlags
		jobDef.Args = append(append([]string{}, jobDef.Args...), parsed.RawJobArgs...)
		job := &jobs.ScheduledJob{
			Project:   workspaceOnceProject(ws),
			Extension: resolved.Extension,
			JobDef:    &jobDef,
			Selection: &wireSelection,
			// The RESOLVED projects, not just their ids. `selection` says WHICH
			// ids are in scope; this is what an extension opens a file with — a
			// name, a workspace-relative path and an absolute one. Without it a
			// subcommand that reads project files had an id list and no way to
			// turn it into a path, so it would have had to scan the tree or
			// shell out to `putnami` — the two things an extension must not do.
			//
			// It is the SAME slice the selection above reports, taken from the
			// one resolver this path already ran: re-resolving here would be the
			// second selection contract the wire block exists to prevent. Its
			// order is the selection's canonical id order rather than the
			// planned path's run order, because an interactive command is one
			// process with no per-project run order to report.
			//
			// Setting it also makes the task identity's scope "workspace", which
			// is what this job has always been: it runs once, for the synthetic
			// workspace project above.
			SelectedProjects: selection.Projects(),
		}
		// Interactive commands are the sole scheduler bypass. Mirror the
		// execution-only global overlay Engine.execute supplies to planned tasks.
		if parsed.Global.MaxParallel > 0 {
			commandParams["max-parallel"] = strconv.Itoa(parsed.Global.MaxParallel)
		} else if parsed.Global.MaxParallelMode != "" {
			commandParams["max-parallel"] = parsed.Global.MaxParallelMode
		}
		if parsed.Global.NoCache {
			commandParams["no-cache"] = true
		}
		// And the SAME negation under the name the subcommand actually declares.
		// See negatedGlobalFlagNames for why `no-cache` alone was not
		// enough. This is the interactive path's copy; the planned path gets it
		// from Engine.execute's execution-only overlay.
		for _, name := range negatedGlobalFlagNames(parsed.OriginalArgs, effectiveFlags) {
			commandParams[name] = false
		}
		// The three selection globals the RESOLVED `selection` block above cannot
		// round-trip. A flag belongs here when resolving it LOSES information the
		// subcommand needs; everything else already travels in `selection`, and
		// duplicating it would invite an extension to re-resolve a selection the
		// orchestrator already resolved — the second selection contract D3 exists
		// to prevent.
		//
		//   - `--projects` RAW, sentinels included (the parser writes "*" for
		//     --all and "[impacted]" for --impacted). Resolution maps many
		//     spellings onto one id set, and a subcommand whose target is a SINGLE
		//     project needs the spelling that was typed — including the two it must
		//     refuse by name.
		//   - `--all`, because it resolves to the same answer as passing nothing:
		//     it is the explicit spelling of the default, so `scoped` stays false
		//     and the resolved block cannot show it.
		//   - `--baseline`, because only `--impacted` consumes it; standing alone
		//     it resolves to nothing and disappears, and a shrink-only architecture
		//     comparison takes it on its own.
		//
		// This is the interactive path ONLY. The planned alias reaches its
		// extension through Engine.Run, which already carries the whole GlobalFlags
		// value, and adding params there would move every planned extension task's
		// cache key for a flag that never changed its work.
		if parsed.Global.Projects != "" {
			commandParams["projects"] = parsed.Global.Projects
		}
		if parsed.Global.All {
			commandParams["all"] = true
		}
		if parsed.Global.Baseline != "" {
			commandParams["baseline"] = parsed.Global.Baseline
		}
		ctx, err = jobs.GrantReleaseSetProviderJob(ctx, job)
		if err != nil {
			iox.Fprintf(stderr, "putnami: %v\n", err)
			return ExitUsage
		}
		versions, err := engine.BuildVersionInfo(ws)
		if err != nil {
			iox.Fprintf(stderr, "putnami: %v\n", err)
			return ExitError
		}
		return runInteractiveExtensionCommand(
			ctx, ws, job, commandParams, versions,
			stdin, stdout, stderr,
		)
	}
	return runPlannedExtensionAlias(ctx, parsed, cfg, resolved, commandParams, wsRoot)
}

// negatedGlobalFlagNames answers the one question the global-flag pass makes
// unanswerable for an extension: which of the SUBCOMMAND's own boolean flags did
// the user negate with a `--no-<flag>` token the HOST consumed as a global?
//
// The convention is not new. buildCommandParams already turns a `--no-<x>` token
// that reaches RawJobArgs into `params["x"] = false`, and every extension SDK
// flag parser spells it the same way (tooling/extension-sdk/cli.ParseFlags).
// What breaks is OWNERSHIP: `--no-cache` is also a CLI-wide global, so
// parseGlobalFlags consumes the token before it can reach RawJobArgs, and the
// only thing the extension received was the framework's own spelling,
// `no-cache: true`. A subcommand that declares a `cache` boolean — as the Cloud
// extension's `ci retry` and `ci start` do — reads `cache` with a `true`
// default, so an explicit `--no-cache` ran WARM and recorded nothing.
//
// Reading the invocation's own tokens rather than a per-flag GlobalFlags field
// keeps this GENERAL: every `--no-<x>` in the global-flag catalog is covered,
// present and future, with nothing to extend alongside it. isGlobalFlag is the
// ownership test, and it is required, not decorative: a `--no-<x>` the host does
// NOT own already survives into RawJobArgs, where buildCommandParams handles it
// — forwarding it here as well would be the second convention for one fact.
//
// The gate is the subcommand's EFFECTIVE flag surface, exactly like the dry-run
// forwarding in buildExtensionCommandParams: a subcommand that never declared
// `<x>` receives nothing, so no unrelated command grows a stray param. An empty
// Type counts as boolean because that is how a manifest spells a bare switch.
func negatedGlobalFlagNames(originalArgs []string, flags map[string]extension.FlagDefinition) []string {
	var names []string
	for _, arg := range originalArgs {
		name, negating := strings.CutPrefix(arg, "--no-")
		if !negating || !isGlobalFlag(arg) {
			continue
		}
		// A VALUE-taking global that merely starts with "--no-" is not a
		// negation of anything: `--no-cache-projects <list>` names projects, and
		// reading it as `cache-projects: false` would hand a subcommand a
		// parameter — and therefore a cache-key input — the user never wrote.
		if spec, known := globalFlagSpec(arg); known && spec.Type == commandmeta.FlagValue {
			continue
		}
		def, declared := flags[name]
		if !declared || (def.Type != "" && def.Type != "boolean") {
			continue
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

func buildExtensionCommandParams(parsed *ParsedArgs, flags map[string]extension.FlagDefinition) map[string]any {
	params := buildCommandParams(parsed.RawJobArgs)
	// Extension command groups such as `putnami cloud publish-config` use
	// command-local flags, but ParseArgs consumes globally-known flags before
	// dispatch. Preserve the common dry-run flag only for commands whose
	// effective flag surface (flat command ⊕ group ⊕ subcommand) declares it so
	// unrelated structured commands do not receive stray params.
	if _, declaresDryRun := flags["dry-run"]; parsed.Global.DryRun && declaresDryRun {
		params["dry-run"] = true
	}
	// Forward the resolved output mode so an extension that wants the selected
	// mode can have it. The reserved global-flag registry forbids a
	// manifest from declaring its own `output` flag, and the CLI consumes
	// --output/--json before dispatch, so the only way the resolved value
	// reaches the extension is through params: re-deliver the canonical mode
	// (post protocolcli.ResolveOutputMode in App.Run; --json arrives here as
	// "json"). Deliberately NOT gated on the effective flag surface like dry-run
	// above — no manifest may declare the flag, so gating on it would forward
	// the mode to nobody, and an extension that never reads the param is
	// unaffected. (Until CLI contract 3 this was phrased as the lossless half of
	// AdaptReservedFlagShadows, which dropped such a flag from an older-contract
	// manifest; adaptation is gone, the forwarding is not.)
	// OutputAuto ("" — no mode selected) forwards nothing so an extension's own
	// default rendering is never overridden, and the Valid() guard keeps
	// unvalidated env/config strings (PUTNAMI_OUTPUT, config output — which
	// bypass ResolveOutputMode) from crossing the process boundary.
	if mode := protocolcli.OutputMode(parsed.Global.Output); mode != protocolcli.OutputAuto && mode.Valid() {
		params["output"] = string(mode)
	}
	return params
}

// extensionAliasPlanSelection carries only the stable owner identity and the
// group/subcommand flag layers into the engine. Engine.Run discovers extensions
// again after before-hooks, then overlays these inherited layers on the fresh
// flat command definition.
func extensionAliasPlanSelection(resolved *extension.ResolvedSubcommand) *engine.PlanExtensionSelection {
	return &engine.PlanExtensionSelection{
		Name:           resolved.Extension.Name,
		Command:        resolved.CommandName,
		InheritedFlags: extension.MergeFlagLayers(resolved.GroupFlags, resolved.Subdef.Flags),
	}
}

// runInteractiveExtensionCommand runs an interactive extension subcommand
// directly — no scheduler, no renderer, no JSONL parsing. The subprocess
// inherits stdin/stdout/stderr so its output appears in the terminal as if
// the user ran a built-in CLI command.
func runInteractiveExtensionCommand(
	ctx context.Context,
	ws *workspace.Workspace,
	job *jobs.ScheduledJob,
	commandParams map[string]any,
	versions jobs.RunVersions,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	configDefaults := map[string]any{}
	if ws.Config != nil {
		configDefaults = ws.Config.GetCommandDefaults(job.JobDef.Name, job.Extension.Name)
	}

	result, err := jobs.RunJobInteractiveWithStreams(
		ctx, ws, job, commandParams, configDefaults, versions, stdin, stdout, stderr,
	)
	if err != nil {
		iox.Fprintf(stderr, "putnami: %v\n", err)
		return ExitError
	}
	switch result.Status {
	case "success":
		return ExitSuccess
	case "canceled":
		return ExitSignalReceived
	default:
		if result.ExitCode != 0 {
			return result.ExitCode
		}
		return ExitError
	}
}

// runPlannedExtensionAlias is the EXTENSION-ALIAS ADAPTER over engine.Run.
// `putnami <group> <sub>` is the extension's flat command with a
// group/subcommand flag surface layered on, so it gets the same lifecycle a bare
// `putnami <command>` gets: session file, remote cache, opportunistic GC,
// successful-run markers, signal/Aborted handling (Ctrl-C is 130, not 1),
// forwarded run exit codes, lifecycle hooks, and the production preflight gate.
// Before this adapter it ran on a private scheduler construction that had none of them.
//
// Two seams are deliberately NOT the terminal path's:
//
//   - Observer stays nil (ADR 0001 §4). The alias dispatches from App.Run before
//     runTerminalSession, so no telemetry client and no consent notice exist at
//     this point; supplying an observer here would report a session the user was
//     never told about, which an earlier design settled against. Only the terminal
//     adapter observes.
//   - ValidateCommandFlags stays nil because the alias already ran the
//     equivalent, at the finer granularity only it can compute: the undeclared
//     flag check above resolves against the subcommand's effective flag surface,
//     and a single flat command has no conflicting-task-flag case.
func runPlannedExtensionAlias(
	ctx context.Context,
	parsed *ParsedArgs,
	cfg *wsproto.Config,
	resolved *extension.ResolvedSubcommand,
	commandParams map[string]any,
	wsRoot string,
) int {
	aliasParsed := *parsed
	aliasParsed.Commands = []string{resolved.CommandName}
	aliasParsed.Subcommand = ""
	aliasParsed.RawJobArgs = promoteProjectSelector(&aliasParsed.Global, aliasParsed.RawJobArgs)

	result, _ := engine.New().Run(ctx, engine.Request{
		WorkspaceRoot: wsRoot,
		Config:        cfg,
		Commands:      aliasParsed.Commands,
		Global:        aliasParsed.Global,
		CommandParams: commandParams,
		// RunMarkerParams is the raw-token map on purpose: commandParams carries
		// the synthesized dry-run/output params this path adds for the job, and a
		// param value's Go type is part of every marker key, so folding those in
		// would change which last-build marker a bare `putnami <group> <sub>`
		// looks up — and, through it, what an unrelated terminal run considers
		// impacted.
		RunMarkerParams: buildCommandParams(aliasParsed.RawJobArgs),
		// One extension, rebound by stable owner identity after before-hooks,
		// with the group/subcommand flag layers overlaid on the fresh command.
		// Planning against the whole discovered set would schedule every other
		// extension's job of the same flat name.
		PlanExtension: extensionAliasPlanSelection(resolved),
		// The alias has always executed under --dry-run, forwarding it to the
		// extension as a job param; --plan is its plan-only preview.
		ExecutesUnderDryRun: true,
		// Lifecycle hooks run for the alias exactly as they do for the flat
		// command it dispatches: the hook keys are the command names in
		// Request.Commands, and this run really does execute that command.
		Hooks: cfg.Hooks,
		// And it gates like that command too: `putnami cloud deploy` under the
		// production profile must meet the same doctor bar a `putnami build`
		// does. See terminalRequest for why this is injected.
		Preflight: doctor.DoctorPreflight,
	}, nil)
	return result.ExitCode
}

// workspaceOnceProject returns a synthetic project representing the workspace
// itself, used when an extension command runs once for the whole workspace
// rather than per-project.
func workspaceOnceProject(ws *workspace.Workspace) *workspace.Project {
	name := ws.Name
	if name == "" {
		name = "workspace"
	}
	return &workspace.Project{
		ID:   name,
		Name: name,
		Path: ".",
	}
}

// printExtensionGroupOrSubcommandHelp routes `putnami <group> [--help]` help:
// per-subcommand help when a subcommand is named, otherwise the group listing.
// The output selector (--output=json|jsonl) opts into the machine-readable help
// contract instead of the human text.
func printExtensionGroupOrSubcommandHelp(extensions []*extension.ExtensionDescription, group, sub, output string) {
	if sub != "" {
		printExtensionSubcommandHelp(extensions, group, sub, output)
		return
	}
	printExtensionGroupHelp(extensions, group, output)
}

// printExtensionGroupHelp lists the subcommands of an extension command group.
// With --output=json|jsonl it emits the subcommands as a machine-readable catalog
// so an agent can enumerate the surface in one call.
func printExtensionGroupHelp(extensions []*extension.ExtensionDescription, group, output string) {
	if format := helpJSONFormat(output); format != "" {
		printExtensionGroupHelpJSON(extensions, group, format)
		return
	}
	subs := extension.ListSubcommands(extensions, group)
	if len(subs) == 0 {
		iox.Fprintf(os.Stdout, "putnami: command group %s has no subcommands\n", group)
		return
	}
	iox.Fprintf(os.Stdout, "Usage: putnami %s <subcommand> [flags]\n\n", group)
	iox.Fprintln(os.Stdout, "Subcommands:")
	for _, s := range subs {
		iox.Fprintf(os.Stdout, "  %s\n", s)
	}
}

// printExtensionGroupHelpJSON emits the group's subcommands as the machine
// catalog: a JSON array under --output=json, one JSON object per line under
// --output=jsonl. Each entry carries enough (command path + description) for an
// agent to enumerate the surface and drill into a subcommand's full help.
func printExtensionGroupHelpJSON(extensions []*extension.ExtensionDescription, group, format string) {
	subs := extension.ListSubcommands(extensions, group)
	summaries := make([]subcommandSummaryJSON, 0, len(subs))
	for _, s := range subs {
		summary := subcommandSummaryJSON{Command: group + " " + s}
		if ext, subdef, ok := extension.LookupSubcommand(extensions, group, s); ok {
			summary.Description = subcommandDescription(ext, subdef)
		}
		summaries = append(summaries, summary)
	}

	enc := newHelpJSONEncoder(format)
	if format == "json" {
		_ = enc.Encode(summaries)
		return
	}
	for _, s := range summaries {
		_ = enc.Encode(s)
	}
}

// printExtensionSubcommandHelp renders per-subcommand help for
// `putnami <group> <sub>`: the subcommand's description, the union of its own
// flags and its flat command target's flags, and any runnable examples. Help is
// rendered straight from the manifest subcommand definition even when the flat
// command target is missing (a nested-parent subcommand or an unimplemented
// command), so the output differs per subcommand instead of degenerating into
// the flat group listing.
func printExtensionSubcommandHelp(extensions []*extension.ExtensionDescription, group, sub, output string) {
	ext, subdef, ok := extension.LookupSubcommand(extensions, group, sub)
	if !ok {
		if helpJSONFormat(output) != "" {
			// Nothing structured to emit for an unknown subcommand; keep the JSON
			// stream on stdout clean and diagnose on stderr.
			iox.Fprintf(os.Stderr, "putnami: unknown subcommand: %s %s\n", group, sub)
			return
		}
		iox.Fprintf(os.Stdout, "putnami: unknown subcommand: %s %s\n", group, sub)
		printExtensionGroupHelp(extensions, group, output)
		return
	}

	if format := helpJSONFormat(output); format != "" {
		_ = newHelpJSONEncoder(format).Encode(buildSubcommandHelpJSON(ext, group, sub, subdef))
		return
	}

	iox.Fprintf(os.Stdout, "Usage: putnami %s %s [flags]\n\n", group, sub)

	description := subcommandDescription(ext, subdef)
	if description != "" {
		iox.Fprintln(os.Stdout, description)
		iox.Fprintln(os.Stdout)
	}

	flags := collectSubcommandFlags(ext, group, subdef)
	if len(flags) > 0 {
		iox.Fprintln(os.Stdout, "Flags:")
		for _, name := range SortedExtensionFlags(flags) {
			flag := flags[name]
			short := ""
			if flag.Short != "" {
				short = flag.Short + ", "
			}
			def := ""
			if flag.Default != nil {
				def = fmt.Sprintf(" (default: %v)", flag.Default)
			}
			desc := flag.Description
			if desc == "" {
				desc = name
			}
			iox.Fprintf(os.Stdout, "  %s--%s  %s%s\n", short, name, desc, def)
		}
		iox.Fprintln(os.Stdout)
	}

	if len(subdef.Examples) > 0 {
		iox.Fprintln(os.Stdout, "Examples:")
		for _, example := range subdef.Examples {
			iox.Fprintf(os.Stdout, "  %s\n", example.Command)
			if example.Description != "" {
				iox.Fprintf(os.Stdout, "      %s\n", example.Description)
			}
		}
		iox.Fprintln(os.Stdout)
	}
}

// subcommandDescription resolves a subcommand's one-line description, falling
// back to the flat command target's description when the subcommand omits its
// own. Shared by the text and JSON help renderers so both agree on the wording.
func subcommandDescription(ext *extension.ExtensionDescription, subdef extension.SubcommandDefinition) string {
	if subdef.Description != "" {
		return subdef.Description
	}
	return ext.Commands[subdef.Command]
}

// collectSubcommandFlags returns a subcommand's effective flag surface, merging
// three layers in increasing precedence: the flat command target's flags, the
// group's shared flags, then the subcommand's own flags. The subcommand wins on
// a name collision and the group overrides the flat command target — identical
// to completion and the runtime param path (ResolvedSubcommand.EffectiveFlags).
// Rendered even when the flat command target is nil (an unimplemented or
// nested-parent subcommand). Shared by the text and JSON help renderers so both
// surface the identical flag set.
func collectSubcommandFlags(ext *extension.ExtensionDescription, group string, subdef extension.SubcommandDefinition) map[string]extension.FlagDefinition {
	flags := make(map[string]extension.FlagDefinition)
	if job := ext.Jobs[subdef.Command]; job != nil {
		maps.Copy(flags, extension.CollectCommandFlags([]*extension.JobDefinition{job}))
	}
	if groupDef, ok := ext.CommandGroups[group]; ok {
		maps.Copy(flags, groupDef.Flags)
	}
	maps.Copy(flags, subdef.Flags)
	return flags
}

// --- Machine-readable help contract ---
//
// These shapes are a stable, additive surface selected by --output=json|jsonl on
// help. An agent driving the CLI reads a command's flags and examples from this
// object instead of scraping the human help text. Human --help is unchanged.

// subcommandHelpJSON is the per-subcommand help object emitted for
// `putnami <group> <sub> --help --output=json|jsonl`.
type subcommandHelpJSON struct {
	Command     string            `json:"command"`
	Description string            `json:"description,omitempty"`
	Flags       []helpFlagJSON    `json:"flags"`
	Examples    []helpExampleJSON `json:"examples"`
}

// helpFlagJSON describes one flag in the structured help contract.
type helpFlagJSON struct {
	Name        string   `json:"name"`
	Short       string   `json:"short,omitempty"`
	Type        string   `json:"type,omitempty"`
	Default     any      `json:"default,omitempty"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Choices     []string `json:"choices,omitempty"`
}

// helpExampleJSON is one runnable example in the structured help contract.
type helpExampleJSON struct {
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
}

// subcommandSummaryJSON is one entry in the group-level catalog emitted for
// `putnami <group> --help --output=json|jsonl`.
type subcommandSummaryJSON struct {
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
}

// helpJSONFormat reports the machine-readable help format selected by --output,
// or "" when help should render as human text. Both "jsonl" and "json" opt into
// the structured contract.
func helpJSONFormat(output string) string {
	switch output {
	case "json", "jsonl":
		return output
	default:
		return ""
	}
}

// newHelpJSONEncoder returns a stdout JSON encoder for the structured help
// contract: indented under "json", compact (one object per line) under "jsonl".
// HTML escaping is disabled so command strings and descriptions stay literal.
func newHelpJSONEncoder(format string) *json.Encoder {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if format == "json" {
		enc.SetIndent("", "  ")
	}
	return enc
}

// buildSubcommandHelpJSON assembles the structured per-subcommand help object
// from the manifest definition, reusing the same description resolution and flag
// union as the human renderer so the two never drift.
func buildSubcommandHelpJSON(ext *extension.ExtensionDescription, group, sub string, subdef extension.SubcommandDefinition) subcommandHelpJSON {
	flags := collectSubcommandFlags(ext, group, subdef)
	flagList := make([]helpFlagJSON, 0, len(flags))
	for _, name := range SortedExtensionFlags(flags) {
		f := flags[name]
		flagList = append(flagList, helpFlagJSON{
			Name:        name,
			Short:       f.Short,
			Type:        f.Type,
			Default:     f.Default,
			Description: f.Description,
			Required:    f.Required,
			Choices:     f.Choices,
		})
	}

	examples := make([]helpExampleJSON, 0, len(subdef.Examples))
	for _, ex := range subdef.Examples {
		examples = append(examples, helpExampleJSON{Command: ex.Command, Description: ex.Description})
	}

	return subcommandHelpJSON{
		Command:     group + " " + sub,
		Description: subcommandDescription(ext, subdef),
		Flags:       flagList,
		Examples:    examples,
	}
}
