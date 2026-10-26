package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	supportproto "go.putnami.dev/protocol/support"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/cachecmd"
	"go.putnami.dev/tooling/cli/internal/commands/ci"
	"go.putnami.dev/tooling/cli/internal/commands/completion"
	"go.putnami.dev/tooling/cli/internal/commands/configcmd"
	"go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/migrate"
	"go.putnami.dev/tooling/cli/internal/commands/sessions"
	"go.putnami.dev/tooling/cli/internal/commands/treecmd"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// init registers every built-in structured command. This is the single place
// a new command (or command group) is wired in; the dispatcher and
// isStructuredCommand both read from the resulting registry.
func init() {
	registerCommand("extensions", cmdExtensions)
	registerCommand("templates", cmdTemplates)
	registerCommand("projects", cmdProjects)
	registerCommand("scopes", cmdScopes)
	registerCommand("init", cmdInit)
	registerCommand("workspace", cmdWorkspace)
	registerCommand("cli", cmdCLI)
	registerCommand("deps", cmdDeps)
	registerCommand("cache", cmdCache)
	registerCommand("config", cmdConfig)
	registerCommand("context", cmdContext)
	registerCommand("migrate", cmdMigrate)
	registerCommand("sessions", cmdSessions)
	registerCommand("tree", cmdTree)
	registerRawCommand("report", cmdReport)
	registerCommand("completion", cmdCompletion)
	registerCommand("install", cmdInstall)
	registerCommand("telemetry", cmdTelemetry)
	registerCommand("help", cmdHelp)
	registerCommand("upgrade", cmdUpgrade)
	registerCommand("mcp", cmdMcp)
	registerCommand("pin", cmdPin)
	registerCommand("doctor", cmdDoctor)
	registerCommand("impact-plan", cmdImpactPlan)
}

// cmdImpactPlan emits the impacted plan of the exact commit range from --base
// to the checked-out HEAD for the command list its one positional names. It is
// the document an extension reads instead of importing the CLI. It owns no
// impact or task planning logic: ci.EmitImpactPlan routes through Engine.Run
// with the impacted planner. The command list is parsed by impactPlanCommands;
// impact-plan is a positional leaf (commandmeta.PositionalLeaf), so the list
// arrives in env.Args.
func cmdImpactPlan(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	parsed, err := parseCatalogCommandFlags("impact-plan", env.Args)
	if err != nil {
		return err
	}
	if env.Sub != "" || len(parsed.positionals) != 1 {
		return usageErrorf("impact-plan takes exactly one command list: run `putnami impact-plan <commands> --base <commit>`")
	}
	if env.Global.Baseline != "" || env.Global.Impacted || env.Global.Projects != "" || env.Global.All {
		return usageErrorf("impact-plan owns impact selection; use --base instead of --baseline, --impacted, --projects, or --all")
	}
	// The aliases of the loaded vocabulary resolve here as they do for
	// `putnami <command>`.
	scope, _ := discoverHelpScope(env.WsRoot, env.Cfg)
	commands, err := impactPlanCommands(parsed.positionals[0], scope.commandAliases(workspaceAliases(env.Cfg)))
	if err != nil {
		return err
	}
	planner := newImpactPlanPlanner(env.WsRoot, env.Cfg, commands, engine.New().Run, env.WorkspaceWarnings())
	document, err := ci.EmitImpactPlan(env.Ctx, env.WsRoot, ci.PlanOptions{
		Base:       parsed.value("--base"),
		Head:       parsed.value("--head"),
		NoCache:    env.Global.NoCache,
		CLIVersion: Version,
	}, planner)
	if err != nil {
		return err
	}
	return ci.RenderImpactPlan(env.OutputFormat, document)
}

// impactPlanCommands parses the command list of impact-plan the way the CLI
// parses `putnami <command[,command...]>`: comma-separated, with the
// vocabulary and workspace aliases resolved, in order. A command
// named twice, and an alias that expands to several commands, are usage
// errors: the plan names each command once, by its own name.
func impactPlanCommands(list string, aliases commandAliases) ([]string, error) {
	commands, err := resolveCommands(list, aliases)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(commands))
	for _, command := range commands {
		if strings.ContainsFunc(command, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
			return nil, usageErrorf("impact-plan: %q in %q is not one command; name each command of the alias instead", command, list)
		}
		if seen[command] {
			return nil, usageErrorf("impact-plan names %s twice in %q", command, list)
		}
		seen[command] = true
	}
	return commands, nil
}

// engineRun names the adapter seam so the exact Engine.Run request can be
// tested without making commands depend on the engine package.
type engineRun func(context.Context, engine.Request, engine.EventSink) (engine.SessionResult, error)

// newImpactPlanPlanner maps the immutable base revision of impact-plan onto the
// ordinary impacted engine plan for commands. The CLI owns this adapter:
// commands receives only its typed result and cannot form an engine cycle. The
// result and every error it returns name commands, the list it planned. The
// engine run is quiet and loads the workspace itself into warnings, the
// command's report: it writes each load warning once on the command's stderr,
// unless --quiet, and the impact-plan envelope carries them.
func newImpactPlanPlanner(wsRoot string, cfg *wsproto.Config, commands []string, run engineRun,
	warnings *workspace.WarningReport,
) ci.Planner {
	planned := append([]string(nil), commands...)
	gate := strings.Join(planned, ",")
	return func(ctx context.Context, baseSHA string, noCache bool) (ci.PlannerResult, error) {
		result, err := run(ctx, engine.Request{
			WorkspaceRoot: wsRoot,
			Config:        cfg,
			Commands:      append([]string(nil), planned...),
			Global: engine.GlobalFlags{
				Impacted: true,
				Baseline: baseSHA,
				NoCache:  noCache,
				Plan:     true,
				Quiet:    true,
			},
			Stdout: io.Discard,
			// The dispatcher reports the error returned below, once.
			ErrorOut:      io.Discard,
			WarningReport: warnings,
		}, nil)
		if err != nil {
			return ci.PlannerResult{}, fmt.Errorf("plan %s: %w", gate, err)
		}
		ws, err := workspace.Load(wsRoot)
		if err != nil {
			return ci.PlannerResult{}, fmt.Errorf("plan %s: load workspace for impact-plan versions: %w", gate, err)
		}
		versions, err := engine.BuildVersionInfo(ws)
		if err != nil {
			return ci.PlannerResult{}, fmt.Errorf("plan %s: impact-plan versions: %w", gate, err)
		}
		return ci.PlannerResult{
			Commands: append([]string(nil), planned...),
			Complete: result.ExitCode == engine.ExitSuccess,
			Projects: result.Projects,
			Jobs:     result.Plan,
			Versions: versions,
		}, nil
	}
}

// cmdDoctor runs the read-only production-readiness preflight over the selected
// projects under the resolved deployment profile (env.Global.EnvProfile), and
// fails with exit 2 when a high/critical finding is present (only reachable
// under --profile production). It takes flags only — no subcommand.
func cmdDoctor(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	if env.Sub != "" {
		return usageErrorf("doctor takes no subcommand: run `putnami doctor [--profile <profile>] [--project <selector>]`")
	}
	if slices.Contains(env.Args, "--machine") {
		if env.Global.ProfileFlag || env.Global.Projects != "" || slices.ContainsFunc(env.Args, func(a string) bool {
			return a == "--project" || strings.HasPrefix(a, "--project=")
		}) {
			return usageErrorf("doctor --machine reports the machine load and takes neither --profile nor --project")
		}
		if len(env.Args) != 1 {
			return usageErrorf("doctor --machine takes no other arguments")
		}
		return doctor.MachineCommand(env.OutputFormat, doctor.NativeLoadReader())
	}
	return doctor.DoctorCommand(env.WsRoot, env.Args, env.Global.Projects, env.Global.EnvProfile, env.OutputFormat)
}

// cmdPin pins, shows, or removes the workspace's CLI version pin.
func cmdPin(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	// `pin <version>` lands the version in Sub (it is not a flag); `pin` and
	// `pin --remove` arrive via Args. Reassemble the full argument list.
	args := env.Args
	if env.Sub != "" {
		args = append([]string{env.Sub}, args...)
	}
	return versioncmd.CLIPinForVersion(env.Ctx, env.WsRoot, args, Version)
}

func cmdExtensions(env *CommandEnv) error {
	if extensions.HasUserScopeFlag(env.Args) {
		return cmdExtensionsUserScope(env)
	}
	switch env.Sub {
	case "install", "":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		if err := lifecycle.ExtensionsInstallNamed(env.Ctx, env.WsRoot, env.Cfg, env.Args, env.OutputFormat, lifecycleEnv(env)); err != nil {
			return err
		}
		// A materialization (`--platform` / `--dest`) installed nothing
		// into this workspace, so there is no new state for the shared AI
		// context to describe — and regenerating it from a foreign platform's
		// artifacts would write that lie into a committed file.
		if extensions.ExtensionsInstallMaterializes(env.Args) {
			return nil
		}
		// Regenerate shared AI context after install. In JSONL mode use the
		// quiet form so the context chatter does not corrupt the stream.
		updatedCfg := wsproto.Load(env.WsRoot)
		_ = regenerateAIContext(env.WsRoot, updatedCfg, env.OutputFormat)
		return nil
	case "update":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		if err := extensions.ExtensionsUpdate(env.Ctx, env.WsRoot, env.Cfg, env.Args, env.OutputFormat); err != nil {
			return err
		}
		updatedCfg := wsproto.Load(env.WsRoot)
		_ = regenerateAIContext(env.WsRoot, updatedCfg, env.OutputFormat)
		return nil
	case "list":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		prepared, _ := lifecycle.ReadPreparationFromContext(env.Ctx)
		return extensions.ExtensionsListPrepared(env.WsRoot, env.Cfg, env.OutputFormat, preparedExtensionAvailability(prepared))
	case "remove":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		return extensions.ExtensionsRemove(env.WsRoot, env.Cfg, env.Args)
	case "prepare":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		return extensions.ExtensionsPrepare(env.Ctx, env.WsRoot, env.Cfg, env.OutputFormat)
	case "validate":
		// Validates an authored manifest from a path, so it needs no workspace.
		return extensions.ExtensionsValidate(env.Args)
	default:
		return usageErrorf("unknown subcommand: extensions %s", env.Sub)
	}
}

// cmdExtensionsUserScope serves `putnami extensions install|list|remove --user`.
// The user scope lives under ~/.putnami/user and exists outside any workspace,
// so these paths never require, read or write one, even when the command runs
// inside a workspace. Install hooks and the shared AI context belong to a
// workspace and do not run.
func cmdExtensionsUserScope(env *CommandEnv) error {
	userRoot, err := extension.ResolveUserScopeRoot()
	if err != nil {
		return err
	}
	switch env.Sub {
	case "install", "":
		return extensions.ExtensionsInstallUser(env.Ctx, userRoot, env.Args, env.OutputFormat, os.Stdout)
	case "list":
		return extensions.ExtensionsListUser(userRoot, env.Args, env.OutputFormat, os.Stdout)
	case "remove":
		return extensions.ExtensionsRemoveUser(userRoot, env.Args, os.Stdout)
	default:
		return usageErrorf("extensions %s does not take %s: the user scope supports install, list and remove", env.Sub, extensions.UserScopeFlag)
	}
}

func preparedExtensionAvailability(report lifecycle.ReadPreparationReport) map[string]string {
	availability := make(map[string]string, len(report.Extensions)+len(report.Issues))
	for _, item := range report.Extensions {
		availability[item.Name] = item.Root
	}
	for _, issue := range report.Issues {
		if issue.Name != "" {
			availability[issue.Name] = ""
		}
	}
	return availability
}

func cmdTemplates(env *CommandEnv) error {
	switch env.Sub {
	case "install", "":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		return extensions.TemplatesInstall(env.Ctx, env.WsRoot, env.Cfg, env.Args, env.OutputFormat)
	case "update":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		return extensions.TemplatesUpdate(env.Ctx, env.WsRoot, env.Cfg, env.Args, env.OutputFormat)
	case "list":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		return extensions.TemplatesList(env.WsRoot, env.Cfg, env.OutputFormat)
	case "remove":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		return extensions.TemplatesRemove(env.WsRoot, env.Cfg, env.Args)
	case "validate":
		// validate, test and package work on an authored template from a path,
		// so they need no workspace.
		return extensions.TemplatesValidate(env.Args)
	case "test":
		channel, args, err := lifecycle.ResolveChannelFlag(env.Args)
		if err != nil {
			return err
		}
		return extensions.TemplatesTest(env.Ctx, args, channel)
	case "package":
		return extensions.TemplatesPackage(env.Ctx, env.Args)
	default:
		return usageErrorf("unknown subcommand: templates %s", env.Sub)
	}
}

func cmdProjects(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "list", "":
		return lifecycle.ProjectsList(env.WsRoot, env.Cfg, env.OutputFormat, env.WorkspaceWarnings())
	case "create":
		return lifecycle.ProjectsCreate(env.Ctx, env.WsRoot, env.Cfg, env.Args, lifecycleEnv(env))
	case "describe":
		return lifecycle.ProjectsDescribe(env.WsRoot, env.Cfg, env.Args, env.OutputFormat, env.WorkspaceWarnings())
	case "sync":
		return lifecycle.ProjectsSync(env.Ctx, env.WsRoot, env.Cfg, env.Args, env.Global.DryRun, lifecycleEnv(env))
	case "tag":
		return lifecycle.ProjectsTag(env.WsRoot, env.Cfg, env.Args, env.WorkspaceWarnings())
	default:
		return usageErrorf("unknown subcommand: projects %s", env.Sub)
	}
}

func cmdScopes(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "list", "":
		return lifecycle.ScopesList(env.WsRoot, env.Cfg, env.OutputFormat, env.WorkspaceWarnings())
	default:
		return usageErrorf("unknown subcommand: scopes %s\n  Available: list", env.Sub)
	}
}

func cmdInit(env *CommandEnv) error {
	return lifecycle.WorkspaceInit(env.Ctx, env.WsRoot, env.Args, lifecycleEnv(env))
}

func cmdWorkspace(env *CommandEnv) error {
	switch env.Sub {
	case "init":
		return lifecycle.WorkspaceInit(env.Ctx, env.WsRoot, env.Args, lifecycleEnv(env))
	case "describe", "":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		return lifecycle.WorkspaceDescribe(env.WsRoot, env.Cfg, env.OutputFormat, env.WorkspaceWarnings())
	default:
		return usageErrorf("unknown subcommand: workspace %s", env.Sub)
	}
}

// cmdCLI manages the installed CLI binaries in .putnami/bin (local) or
// ~/.putnami/bin (--global), so it needs no workspace when --global, mirroring
// `upgrade --global`.
func cmdCLI(env *CommandEnv) error {
	switch env.Sub {
	case "":
		return missingSubcommandError("cli")
	case "list":
		binDir, err := cliBinDir(env)
		if err != nil {
			return err
		}
		return versioncmd.VersionList(env.Ctx, binDir)
	case "use":
		binDir, err := cliBinDir(env)
		if err != nil {
			return err
		}
		return versioncmd.VersionUse(env.Ctx, binDir, firstPositionalArg(env.Args), suppliedFlag(env.Args, "--global", "-g"))
	default:
		return usageErrorf("unknown subcommand: cli %s\n  Available: list, use", env.Sub)
	}
}

// versionGraphRefresh is the outcome of refreshVersionGraph: err is why the
// refresh failed, nil when it succeeded or was not needed, and recorded is the
// view a version can still be read from after a failure.
type versionGraphRefresh struct {
	err      error
	recorded workspace.RecordedView
}

// refreshVersionGraph refreshes the recorded dependency graph before a version
// is computed from a support catalog, and reads the recorded view when the
// refresh fails. The catalog promotes an unlisted project a stable one depends
// on, and a Go dependency edge exists only in the recorded provider view, so a
// version read from a missing or outdated view would differ between a fresh
// clone and a machine that ran a build. A workspace without a catalog never
// reads an edge and is not refreshed. The version.lines host RPC method calls
// it.
func refreshVersionGraph(ctx context.Context, wsRoot string, cfg *wsproto.Config) versionGraphRefresh {
	if _, err := os.Stat(filepath.Join(wsRoot, supportproto.CatalogFilename)); err != nil {
		return versionGraphRefresh{}
	}
	err := synchronizeWorkspaceGraph(ctx, wsRoot, cfg)
	if err == nil {
		return versionGraphRefresh{}
	}
	return versionGraphRefresh{err: err, recorded: workspace.RecordedIndexView(wsRoot, time.Now())}
}

// refusal is the error of a version that must not be read from the recorded
// view after the refresh failed.
func (r versionGraphRefresh) refusal() error {
	return fmt.Errorf("version: %s reads the dependency graph, which could not be refreshed: %w",
		supportproto.CatalogFilename, r.err)
}

// cliBinDir resolves the directory holding the installed putnami CLI binaries:
// ~/.putnami/bin under --global (no workspace required), otherwise the
// workspace's .putnami/bin (which requires a workspace).
func cliBinDir(env *CommandEnv) (string, error) {
	if suppliedFlag(env.Args, "--global", "-g") {
		return resolveBinDir("", env.Args), nil
	}
	if err := env.requireWorkspace(); err != nil {
		return "", err
	}
	return resolveBinDir(env.WsRoot, env.Args), nil
}

func cmdDeps(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "install", "":
		return lifecycle.DepsInstall(env.Ctx, env.WsRoot, env.Cfg, env.Global.FilterTag, env.Global.ExcludeTag, lifecycleEnv(env))
	case "add":
		return lifecycle.DepsAdd(env.Ctx, env.WsRoot, env.Args, env.Global.Projects, env.Global.DryRun, lifecycleEnv(env))
	case "remove", "rm":
		return lifecycle.DepsRemove(env.Ctx, env.WsRoot, env.Args, env.Global.Projects, env.Global.DryRun, lifecycleEnv(env))
	case "prune":
		return lifecycle.DepsPrune(env.Ctx, env.WsRoot, env.Global.Projects, env.Global.DryRun, lifecycleEnv(env))
	default:
		return usageErrorf("unknown subcommand: deps %s\n  Usage: putnami deps <install|add|remove|prune> [module@version] [--projects <name>]\n  To upgrade dependencies, use: putnami upgrade --deps", env.Sub)
	}
}

func cmdCache(env *CommandEnv) error {
	if env.Sub == "" {
		// Every subcommand of cache deletes or rebuilds shared state, so a bare
		// `cache`, which a reader types to discover them, runs none of them.
		return missingSubcommandError("cache")
	}
	if env.Global.DryRun && (env.Sub == "clean" || env.Sub == "gc" || env.Sub == "verify") {
		return usageErrorf("cache %s does not support --dry-run; remove --dry-run to run it", env.Sub)
	}
	switch env.Sub {
	case "clean":
		// `clean --all` and `gc` act on the machine-global store and need no
		// workspace; a bare `clean` targets THIS repo's store and so does. The
		// extension cacheClean fan-out is a no-op without a workspace, so `clean
		// --all` outside one still wipes the machine-global store.
		if env.Global.All {
			return cachecmd.CacheClean(env.Ctx, env.WsRoot, true, env.Cfg, env.OutputFormat)
		}
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		return cachecmd.CacheClean(env.Ctx, env.WsRoot, false, env.Cfg, env.OutputFormat)
	case "gc":
		return cachecmd.CacheGC(env.Ctx, env.WsRoot, env.Cfg, env.OutputFormat)
	case "verify":
		if err := env.requireWorkspace(); err != nil {
			return err
		}
		if len(env.Args) != 0 {
			return usageErrorf("cache verify takes no positional arguments")
		}
		if env.Global.NoCache {
			return usageErrorf("cache verify cannot run with --no-cache")
		}
		commands := cacheVerifyCommands(env.WsRoot)
		return cachecmd.CacheVerifyCommand(env.Ctx, env.WsRoot, commands,
			env.OutputFormat, newCacheVerifyRunner(env.Cfg, env.Global, commands, engine.New().Run, env.WorkspaceWarnings()))
	default:
		return usageErrorf("unknown subcommand: cache %s\n  Available: clean, gc, verify", env.Sub)
	}
}

// cacheVerifyCommands returns the commands cache verify runs in the workspace
// at wsRoot: its gate, the commands its generated guidance tells a contributor
// to run (agentctx.GateTasks). The core names no command of its own.
func cacheVerifyCommands(wsRoot string) []string {
	return strings.Split(agentctx.GateTasks(wsRoot), ",")
}

// newCacheVerifyRunner is the CLI-owned adapter from the cache verifier onto
// Engine.Run for commands. The checker observes the real plan, keys,
// scheduler, capture, and restore paths without becoming a second execution
// composition. The three runs load the same HEAD, so only the first run
// reports its workspace load into warnings, the command's report: it writes
// each load warning once on the command's stderr, unless --quiet, and the
// cache verify envelope lists each warning once.
func newCacheVerifyRunner(
	cfg *wsproto.Config,
	global GlobalFlags,
	commands []string,
	run engineRun,
	warnings *workspace.WarningReport,
) cachecmd.CacheVerifyRunner {
	commands = append([]string(nil), commands...)
	reported := false
	return func(ctx context.Context, workspaceRoot, storeRoot string) (cachecmd.CacheVerifyEngineResult, error) {
		flags := global
		flags.Plan = false
		flags.DryRun = false
		flags.NoCache = false
		// The scoped form too: cache verification observes the real lookup and
		// restore paths, and a project the caller scoped out would be verified
		// against a cache it never consulted.
		flags.NoCacheProjects = ""
		flags.CacheTrust = "none"
		flags.Watch = false
		flags.Output = ""
		flags.JSON = false
		flags.Quiet = true
		flags.Verbose = false
		flags.Debug = false
		flags.MaxParallel = 1
		flags.MaxParallelMode = ""
		// No selection means every project, as --all sets it: the runs plan in
		// a staged worktree, so the default selection, the projects under the
		// working directory, would match none.
		if flags.Projects == "" && !flags.Impacted && !flags.All && flags.FilterTag == "" {
			flags.All = true
			flags.Projects = "*"
		}
		var hooks *wsproto.HooksConfig
		if cfg != nil {
			hooks = cfg.Hooks
		}
		request := engine.Request{
			WorkspaceRoot:    workspaceRoot,
			Config:           cfg,
			Commands:         append([]string(nil), commands...),
			Global:           flags,
			Hooks:            hooks,
			Preflight:        doctor.DoctorPreflight,
			EphemeralSession: true,
			Stdout:           io.Discard,
			// The dispatcher reports the returned error, once.
			ErrorOut: io.Discard,
		}
		request.CacheVerification = &engine.CacheVerificationRequest{StoreRoot: storeRoot}
		// A later run is quiet and gets no report: the engine makes one that
		// writes nothing.
		if !reported {
			reported = true
			request.WarningReport = warnings
		}
		result, err := run(ctx, request, output.NewPassthroughRenderer())
		return cachecmd.CacheVerifyEngineResult{
			ExitCode: result.ExitCode,
			Plan:     result.Plan,
			Projects: result.Projects,
			Results:  result.Results,
		}, err
	}
}

func cmdConfig(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "show", "":
		return configcmd.ConfigShow(env.WsRoot, env.Cfg, env.OutputFormat)
	case "set":
		return configcmd.ConfigSet(env.WsRoot, env.Args)
	default:
		return usageErrorf("unknown subcommand: config %s", env.Sub)
	}
}

func cmdContext(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "generate", "":
		return agentctx.ContextGenerate(env.WsRoot, env.Cfg, env.Args)
	case "pack":
		return agentctx.ContextPackCommand(env.WsRoot, env.Args, env.OutputFormat, Version)
	case "map":
		// Project selection is CLI-global (--projects/--impacted/--baseline), so
		// it arrives already parsed; the command never sees the flag parser.
		return agentctx.ContextMapCommand(env.WsRoot, agentctx.ContextMapOptions{
			Projects:     env.Global.Projects,
			Impacted:     env.Global.Impacted,
			Baseline:     env.Global.Baseline,
			OutputFormat: env.OutputFormat,
		})
	default:
		return usageErrorf("unknown subcommand: context %s", env.Sub)
	}
}

func cmdMigrate(env *CommandEnv) error {
	// migrate runs against the current directory when no workspace is found,
	// so vnext can read a lock that every other command refuses to load.
	root := env.WsRoot
	if root == "" {
		cwd, _ := os.Getwd()
		root = cwd
	}
	switch env.Sub {
	case "vnext":
		return cmdMigrateVNext(env, root)
	default:
		return usageErrorf("unknown migration source: %s\n  Available: vnext", env.Sub)
	}
}

// cmdMigrateVNext binds `migrate vnext`'s two modes. They are mutually
// exclusive rather than "last one wins": --check and --apply differ in whether
// the lock is written, so an invocation carrying both is ambiguous about the
// one thing that matters. Neither flag means --check, the read-only mode.
func cmdMigrateVNext(env *CommandEnv, root string) error {
	parsed, err := parseCatalogCommandFlags("migrate vnext", env.Args)
	if err != nil {
		return err
	}
	check, apply := parsed.has("--check"), parsed.has("--apply")
	if check && apply {
		return usageErrorf("migrate vnext: --check and --apply are mutually exclusive")
	}
	return migrate.MigrateVNext(root, apply, env.OutputFormat)
}

func cmdSessions(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "list", "":
		parsed, err := parseCatalogCommandFlags("sessions list", env.Args)
		if err != nil {
			return err
		}
		if len(parsed.positionals) != 0 {
			return usageErrorf("sessions list takes no positional arguments: narrow the listing with --revision <sha>")
		}
		return sessions.SessionsListRevision(env.WsRoot, env.OutputFormat, parsed.value("--revision"))
	case "inspect":
		parsed, err := parseCatalogCommandFlags("sessions inspect", env.Args)
		if err != nil {
			return err
		}
		if ref := parsed.value("--run"); ref != "" {
			if len(parsed.positionals) != 0 {
				return usageErrorf("sessions inspect takes either a session id or --run <ref>, not both")
			}
			return sessions.SessionsInspectRun(env.Ctx, env.WsRoot, env.Cfg, ref, env.OutputFormat)
		}
		return sessions.SessionsInspect(env.WsRoot, parsed.positionals, env.OutputFormat)
	case "export":
		return cmdSessionsExport(env)
	case "summary":
		return cmdSessionsSummary(env)
	case "replay":
		parsed, err := parseCatalogCommandFlags("sessions replay", env.Args)
		if err != nil {
			return err
		}
		if len(parsed.positionals) != 0 || parsed.value("--session") == "" {
			return usageErrorf("sessions replay requires --session <exact-id>")
		}
		return engine.New().ReplaySession(env.Ctx, env.WsRoot, env.Cfg, parsed.value("--session"))
	default:
		return usageErrorf("unknown subcommand: sessions %s", env.Sub)
	}
}

// cmdTree answers questions about the worktree the CLI was invoked in.
//
// It deliberately does NOT require a workspace: fingerprinting a tree is a pure
// git operation, and its first callers run it inside throwaway repositories that
// carry no putnami.json. The digest covers the tree the CALLER is in, resolved
// from the process working directory rather than from env.WsRoot, so a workspace
// nested in a larger repository still fingerprints the repository it sits in.
func cmdTree(env *CommandEnv) error {
	if env.Sub == "" {
		return missingSubcommandError("tree")
	}
	if env.Sub != "fingerprint" {
		return usageErrorf("unknown subcommand: tree %s\n  Available: fingerprint", env.Sub)
	}
	parsed, err := parseCatalogCommandFlags("tree fingerprint", env.Args)
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 0 {
		return usageErrorf("tree fingerprint takes no arguments: it fingerprints the worktree it runs in")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve the working directory: %w", err)
	}
	return treecmd.Fingerprint(workingDirectory, env.OutputFormat)
}

// cmdSessionsExport streams the recorded session documents. Its output is the
// raw JSONL stream in every mode: --output=json|jsonl still wraps it in the
// shared result envelope like every other `sessions` subcommand, because
// `sessions` is a registerCommand and runStructuredCommand captures its stdout.
func cmdSessionsExport(env *CommandEnv) error {
	parsed, err := parseCatalogCommandFlags("sessions export", env.Args)
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 0 {
		return usageErrorf("sessions export takes no positional arguments: narrow the stream with --since <timestamp>")
	}
	return sessions.SessionsExport(env.WsRoot, parsed.value("--since"))
}

// cmdSessionsSummary prints the recorded sessions reduced to one ledger row
// each. It shares `sessions export`'s ordering and deduplication, so the two
// surfaces can never disagree about which runs a worktree recorded.
func cmdSessionsSummary(env *CommandEnv) error {
	parsed, err := parseCatalogCommandFlags("sessions summary", env.Args)
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 0 {
		return usageErrorf("sessions summary takes no positional arguments: narrow the listing with --since <timestamp> or --command <list>")
	}
	return sessions.SessionsSummary(env.WsRoot, parsed.value("--since"), parsed.value("--command"), parsed.has("--by-digest"), env.OutputFormat)
}

// cmdReport reads back a finished run's recorded report. It is the contracted
// handoff that replaces guessing which session directory is new: with no
// argument it reads the newest CLI-driven run's report, and --session names one
// exactly. Registered with registerRawCommand because --output=json prints the
// recorded contract document itself rather than a result envelope.
func cmdReport(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	if env.Sub != "" {
		return usageErrorf("report takes no subcommand: run `putnami report [--session <id>]`")
	}
	parsed, err := parseCatalogCommandFlags("report", env.Args)
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 0 {
		return usageErrorf("report takes no positional arguments: name a report with --session <id>")
	}
	return sessions.ReportShow(env.WsRoot, parsed.value("--session"), env.OutputFormat)
}

// warningWriter is where a command that reads the workspace writes the
// warnings the load recorded: stderr, which keeps stdout and the machine
// envelope clean, or nil under --quiet, as for the engine's own warnings.
func warningWriter(env *CommandEnv) io.Writer {
	if env.Global.Quiet {
		return nil
	}
	return os.Stderr
}

func cmdCompletion(env *CommandEnv) error {
	shell := env.Sub
	if shell == "" && len(env.Args) > 0 {
		shell = env.Args[0]
	}
	warnings := env.WorkspaceWarnings()
	switch shell {
	case "bash":
		completion.CompletionBash(os.Stdout, env.WsRoot, env.Cfg, warnings)
	case "zsh":
		completion.CompletionZsh(os.Stdout, env.WsRoot, env.Cfg, warnings)
	case "fish":
		completion.CompletionFish(os.Stdout, env.WsRoot, env.Cfg, warnings)
	default:
		return usageErrorf("shell required: completion <bash|zsh|fish>")
	}
	return nil
}

func cmdInstall(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	return lifecycle.Install(env.Ctx, env.WsRoot, env.Cfg, env.Args, lifecycleEnv(env))
}

func cmdTelemetry(env *CommandEnv) error {
	switch env.Sub {
	case "on":
		return configcmd.TelemetryOn()
	case "off":
		return configcmd.TelemetryOff()
	case "status", "":
		return configcmd.TelemetryStatus()
	case "show":
		return configcmd.TelemetryShow(env.OutputFormat)
	default:
		return usageErrorf("unknown subcommand: telemetry %s\n  Available: on, off, status, show", env.Sub)
	}
}

func cmdHelp(env *CommandEnv) error {
	// Check for --man and --markdown in raw args.
	for _, a := range env.Args {
		switch a {
		case "--man":
			printHelpMan(os.Stdout, newHelpScope(env.WsRoot, env.Cfg))
			return nil
		case "--markdown":
			printHelpMarkdown(os.Stdout, newHelpScope(env.WsRoot, env.Cfg))
			return nil
		}
	}
	if env.Sub != "" {
		nestedSub := ""
		if len(env.Args) > 0 && !strings.HasPrefix(env.Args[0], "-") {
			nestedSub = env.Args[0]
		}
		command := resolveAliasOrSelf(env.Sub, commandAliases{user: env.Cfg.Aliases})
		if err := refuseHelpOfRemovedCoreRoot(env, command); err != nil {
			return err
		}
		if err := refuseRemovedCoreSubcommand(command, nestedSub); err != nil {
			return err
		}
		// The aliases of a loaded vocabulary come first, so the command is
		// resolved again once the help scope knows them.
		scope, extensions := discoverHelpScope(env.WsRoot, env.Cfg)
		command = resolveAliasOrSelf(env.Sub, scope.commandAliases(env.Cfg.Aliases))
		switch {
		case isStructuredCommand(command) || nestedSub != "":
			printSubcommandHelp(command, nestedSub, scope)
		case commandGroupsFor(extensions, env.WsRoot, false)[command]:
			// A command group of a loaded extension, or a collaboration
			// contract, is no job command: it prints its subcommands.
			printCommandGroupHelp(env.Ctx, os.Stdout, env.WsRoot, env.Cfg, extensions, command, "", env.OutputFormat)
		case env.WsRoot == "":
			// A job command runs only in a workspace, so its help says so.
			return env.requireWorkspace()
		default:
			return printDeclaredJobCommandHelp(command, extensions, scope)
		}
	} else {
		printHelp(newHelpScope(env.WsRoot, env.Cfg))
	}
	return nil
}

func cmdUpgrade(env *CommandEnv) error {
	flags, err := parseUpgradeFlags(env.Args)
	if err != nil {
		return err
	}
	flags.DryRun = flags.DryRun || env.Global.DryRun
	flags.ContinueOnError = env.Global.ContinueOnErr
	flags.Providers = env.Global.Providers
	cliOnly := false
	if flags.FromSource {
		// Building from source needs the workspace's CLI source tree even with
		// --global, which then only retargets where the built binary installs.
		if err := env.requireWorkspace(); err != nil {
			return err
		}
	} else if suppliedFlag(env.Args, "--global", "-g") && env.WsRoot == "" {
		// --global retargets the CLI phase at the ~/.putnami/bin install
		// (the one install.sh manages), so it needs no workspace. Without
		// one, the workspace phases cannot run: asking for them explicitly
		// is an error, and the default full upgrade narrows to the CLI.
		if flags.Extensions || flags.Deps {
			return usageErrorf("--extensions/--deps need a workspace; outside one, --global only upgrades the CLI")
		}
		cliOnly = !flags.CLI
		flags.CLI = true
	} else if err := env.requireWorkspace(); err != nil {
		return err
	}
	binDir := resolveBinDir(env.WsRoot, env.Args)
	err = lifecycle.Upgrade(env.Ctx, env.WsRoot, env.Cfg, Version, binDir, flags, lifecycleEnv(env))
	if err == nil && cliOnly {
		iox.Fprintln(os.Stdout, "  No workspace here, so only the global CLI was upgraded. Run putnami upgrade inside a workspace to update extensions and dependencies.")
	}
	return err
}

// cmdMcp serves the MCP server over stdio (the bare form MCP clients spawn)
// or, with the install subcommand, writes the .mcp.json registration. A
// workspace is required since every tool operates on one.
func cmdMcp(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "":
		// Serving takes over the process for the life of the agent session,
		// speaking JSON-RPC 2.0 on stdin/stdout. It is the one `mcp` subcommand
		// implemented in the shell rather than in internal/commands; mcp_serve.go
		// explains why (the mcp → engine → commands cycle A4/#2643 would close).
		return mcpServe(env.Ctx, env.WsRoot, env.Cfg, Version)
	case "install":
		return agentctx.MCPInstall(env.WsRoot)
	default:
		return usageErrorf("unknown subcommand: mcp %s\n  Available: install (bare `putnami mcp` serves over stdio)", env.Sub)
	}
}
