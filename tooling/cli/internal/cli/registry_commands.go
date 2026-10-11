package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	supportproto "go.putnami.dev/protocol/support"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/cachecmd"
	"go.putnami.dev/tooling/cli/internal/commands/ci"
	"go.putnami.dev/tooling/cli/internal/commands/completion"
	"go.putnami.dev/tooling/cli/internal/commands/composecmd"
	"go.putnami.dev/tooling/cli/internal/commands/configcmd"
	"go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/migrate"
	"go.putnami.dev/tooling/cli/internal/commands/qualifycmd"
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
	registerCommand("version", cmdVersion)
	registerCommand("deps", cmdDeps)
	registerCommand("cache", cmdCache)
	registerCommand("infra", cmdInfra)
	registerCommand("config", cmdConfig)
	registerCommand("context", cmdContext)
	registerCommand("migrate", cmdMigrate)
	registerCommand("sessions", cmdSessions)
	registerCommand("tree", cmdTree)
	// Raw: the execute skill and finalize-pr.sh parse the verdict document.
	registerRawSubcommand("tree", "verify")
	// Raw: a composition streams its start document before it runs and its exit
	// document when it stops, so its stdout cannot be captured until exit.
	registerRawCommand("compose", cmdCompose)
	registerCommand("qualify", cmdQualify)
	registerRawCommand("report", cmdReport)
	registerCommand("completion", cmdCompletion)
	registerCommand("install", cmdInstall)
	registerCommand("telemetry", cmdTelemetry)
	registerCommand("help", cmdHelp)
	registerCommand("upgrade", cmdUpgrade)
	registerCommand("dev", cmdDev)
	registerCommand("mcp", cmdMcp)
	registerCommand("pin", cmdPin)
	registerCommand("doctor", cmdDoctor)
	registerCommand("change-plan", cmdChangePlan)
	registerCommand("impact-plan", cmdImpactPlan)
}

// cmdChangePlan emits the immutable, exact-revision CI admission document.
// It deliberately owns no impact or task planning logic: ci.EmitChangePlan
// routes through Engine.Run with the same impacted planner as lint/test/build,
// and projects the ImpactPlan impact-plan would emit for changePlanCommands.
func cmdChangePlan(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	if env.Sub != "" {
		return usageErrorf("change-plan takes no subcommand: run `putnami change-plan --base <commit>`")
	}
	parsed, err := parseCatalogCommandFlags("change-plan", env.Args)
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 0 {
		return usageErrorf("change-plan takes no positional arguments: run `putnami change-plan --base <commit>`")
	}
	if env.Global.Baseline != "" || env.Global.Impacted || env.Global.Projects != "" || env.Global.All {
		return usageErrorf("change-plan owns impact selection; use --base instead of --baseline, --impacted, --projects, or --all")
	}
	planner := newChangePlanPlanner(env.WsRoot, env.Cfg, engine.New().Run)
	document, err := ci.EmitChangePlan(env.Ctx, env.WsRoot, ci.PlanOptions{
		Base:       parsed.value("--base"),
		Head:       parsed.value("--head"),
		NoCache:    env.Global.NoCache,
		CLIVersion: Version,
	}, planner)
	if err != nil {
		return err
	}
	return ci.RenderChangePlan(env.OutputFormat, document)
}

// cmdImpactPlan emits the impacted plan of the exact commit range from --base
// to the checked-out HEAD for the command list its one positional names. It is
// the document an extension reads instead of importing the CLI. Like
// change-plan, it owns no impact or task planning logic: ci.EmitImpactPlan
// routes through Engine.Run with the impacted planner. The command list is
// parsed by impactPlanCommands; impact-plan is a positional leaf
// (commandmeta.PositionalLeaf), so the list arrives in env.Args.
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
	commands, err := impactPlanCommands(parsed.positionals[0], env.Cfg)
	if err != nil {
		return err
	}
	planner := newImpactPlanPlanner("impact-plan", env.WsRoot, env.Cfg, commands, refuseUndeclaredCommands(commands, engine.New().Run))
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
// parses `putnami <command[,command...]>`: comma-separated, with built-in and
// workspace aliases resolved, in order. A command named twice, and an alias
// that expands to several commands, are usage errors: the plan names each
// command once, by its own name.
func impactPlanCommands(list string, cfg *wsproto.Config) ([]string, error) {
	var aliases map[string]string
	if cfg != nil {
		aliases = cfg.Aliases
	}
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

// changePlanEngineRun names the adapter seam so the exact Engine.Run request
// can be tested without making commands depend on the engine package.
type changePlanEngineRun func(context.Context, engine.Request, engine.EventSink) (engine.SessionResult, error)

// changePlanCommands is the quality gate the change plan projects: the
// command list CI runs, in the same order.
//
// The two must not drift. The change plan is what a reviewer reads to decide
// what CI will do with a revision, so a command CI runs and the plan omits is a
// task nobody reviewed, and a command the plan shows and CI skips is a check
// nobody performed. `validate-workspace` is not listed: the manifest that
// declares both commands plans it from `validate` through `alsoRuns`, so the
// planned session runs it all the same.
//
// It stays a literal here rather than a read of a CI document: the planner
// must answer for a workspace whose CI document is absent or unreadable, and
// planning nothing at all would report an empty change plan as a legitimate
// "nothing to do". TestChangePlanCommandsMatchTheCIQualityJob holds it equal
// to the gate the generated guidance derives for this workspace.
var changePlanCommands = []string{"lint", "test", "build", "validate"}

// newChangePlanPlanner is the impact planner of changePlanCommands, the one
// change-plan runs.
func newChangePlanPlanner(wsRoot string, cfg *wsproto.Config, run changePlanEngineRun) ci.Planner {
	return newImpactPlanPlanner("change-plan", wsRoot, cfg, changePlanCommands, run)
}

// newImpactPlanPlanner maps the immutable base revision of command (impact-plan
// or change-plan) onto the ordinary impacted engine plan for commands. The CLI
// owns this adapter: commands receives only its typed result and cannot form
// an engine cycle. The result and every error it returns name commands, the
// list it planned.
func newImpactPlanPlanner(command, wsRoot string, cfg *wsproto.Config, commands []string, run changePlanEngineRun) ci.Planner {
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
		}, nil)
		if err != nil {
			return ci.PlannerResult{}, fmt.Errorf("plan %s: %w", gate, err)
		}
		ws, err := workspace.Load(wsRoot)
		if err != nil {
			return ci.PlannerResult{}, fmt.Errorf("plan %s: load workspace for %s versions: %w", gate, command, err)
		}
		versions, err := engine.BuildVersionInfo(ws)
		if err != nil {
			return ci.PlannerResult{}, fmt.Errorf("plan %s: %s versions: %w", gate, command, err)
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

// refuseUndeclaredCommands is the engine run of impact-plan. It refuses, with a
// usage error, every command of commands that no discovered extension declares
// as a job. The core declares no job, so such a command plans zero tasks, and a
// caller that admits a change from the plan would read "nothing to run". The
// check reads the job map the engine plans from, after extension discovery and
// before selection. A declared command that plans zero tasks because nothing it
// serves is impacted stays a valid empty plan. change-plan does not use it.
func refuseUndeclaredCommands(commands []string, run changePlanEngineRun) changePlanEngineRun {
	requested := append([]string(nil), commands...)
	return func(ctx context.Context, req engine.Request, sink engine.EventSink) (engine.SessionResult, error) {
		req.ValidateCommandFlags = func(discovered *extension.DiscoveryResult, _ GlobalFlags) error {
			var extensions []*extension.ExtensionDescription
			if discovered != nil {
				extensions = discovered.Extensions
			}
			jobMap := extension.BuildJobMap(extensions)
			var undeclared []string
			for _, command := range requested {
				if len(jobMap[command]) == 0 {
					undeclared = append(undeclared, command)
				}
			}
			if len(undeclared) == 0 {
				return nil
			}
			return usageErrorf("no extension of this workspace declares %s, so impact-plan cannot plan it; "+
				"%s lists the commands that moved to an extension",
				strings.Join(undeclared, ", "), commandmeta.CommandsThatLeftTheCoreURL)
		}
		return run(ctx, req, sink)
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
		if err := extensions.ExtensionsInstall(env.Ctx, env.WsRoot, env.Cfg, env.Args, env.OutputFormat); err != nil {
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
		return lifecycle.ProjectsList(env.WsRoot, env.Cfg, env.OutputFormat)
	case "create":
		return lifecycle.ProjectsCreate(env.Ctx, env.WsRoot, env.Cfg, env.Args, env.Global.Verbose, lifecycleEnv(env))
	case "describe":
		return lifecycle.ProjectsDescribe(env.WsRoot, env.Cfg, env.Args, env.OutputFormat)
	case "sync":
		return lifecycle.ProjectsSync(env.Ctx, env.WsRoot, env.Cfg, env.Args, env.Global.DryRun, lifecycleEnv(env))
	case "tag":
		return lifecycle.ProjectsTag(env.WsRoot, env.Cfg, env.Args)
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
		return lifecycle.ScopesList(env.WsRoot, env.Cfg, env.OutputFormat)
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
		return lifecycle.WorkspaceDescribe(env.WsRoot, env.Cfg, env.OutputFormat)
	default:
		return usageErrorf("unknown subcommand: workspace %s", env.Sub)
	}
}

func cmdVersion(env *CommandEnv) error {
	// list/use manage the installed CLI binaries in .putnami/bin (local) or
	// ~/.putnami/bin (--global) and so do not need a workspace when --global,
	// mirroring `upgrade --global`. get/set/bump/tag operate on workspace,
	// scope, and project release versions and always require a workspace.
	switch env.Sub {
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
	}

	if err := env.requireWorkspace(); err != nil {
		return err
	}
	if env.Sub == "get" || env.Sub == "" || env.Sub == "tag" {
		if err := synchronizeVersionGraph(env); err != nil {
			return err
		}
	}
	switch env.Sub {
	case "get", "":
		return versioncmd.VersionGet(env.WsRoot, env.Args, env.OutputFormat)
	case "tag":
		args := env.Args
		if env.Global.DryRun {
			args = append(append([]string{}, args...), "--dry-run")
		}
		return versioncmd.VersionTag(env.Ctx, env.WsRoot, args)
	default:
		return usageErrorf("unknown subcommand: version %s\n  Available: get, tag, list, use", env.Sub)
	}
}

// synchronizeVersionGraph refreshes the recorded dependency graph before a
// version is computed from a support catalog. The catalog promotes an unlisted
// project a stable one depends on, and a Go dependency edge exists only in the
// recorded provider view, so a version read from a missing or outdated view
// would differ between a fresh clone and a machine that ran a build. A
// workspace without a catalog never reads an edge and is not refreshed.
//
// When the refresh fails, `version get` still answers from a recorded view, and
// says so, because it is a recovery command that must work offline or with a
// broken provider. `version tag` writes a release and refuses.
func synchronizeVersionGraph(env *CommandEnv) error {
	if _, err := os.Stat(filepath.Join(env.WsRoot, supportproto.CatalogFilename)); err != nil {
		return nil
	}
	err := synchronizeWorkspaceGraph(env.Ctx, env.WsRoot, env.Cfg)
	if err == nil {
		return nil
	}
	if view := workspace.RecordedIndexView(env.WsRoot, time.Now()); env.Sub != "tag" && view.Usable() {
		iox.Fprintf(os.Stderr, "putnami: version: the dependency graph could not be refreshed (%v); "+
			"answering from the recorded view observed at %s\n", err, view.ObservedAt)
		return nil
	}
	return fmt.Errorf("version: %s reads the dependency graph, which could not be refreshed: %w",
		supportproto.CatalogFilename, err)
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
		return lifecycle.DepsAdd(env.Ctx, env.WsRoot, env.Args, env.Global.Projects, lifecycleEnv(env))
	case "remove", "rm":
		return lifecycle.DepsRemove(env.Ctx, env.WsRoot, env.Args, env.Global.Projects, lifecycleEnv(env))
	case "prune":
		return lifecycle.DepsPrune(env.Ctx, env.WsRoot, env.Global.Projects, env.Global.DryRun, lifecycleEnv(env))
	default:
		return usageErrorf("unknown subcommand: deps %s\n  Usage: putnami deps <install|add|remove|prune> [module@version] [--projects <name>]\n  To upgrade dependencies, use: putnami upgrade --deps", env.Sub)
	}
}

func cmdCache(env *CommandEnv) error {
	if env.Global.DryRun && (env.Sub == "" || env.Sub == "clean" || env.Sub == "gc" || env.Sub == "verify") {
		subcommand := env.Sub
		if subcommand == "" {
			subcommand = "clean"
		}
		return usageErrorf("cache %s does not support --dry-run; remove --dry-run to run it", subcommand)
	}
	switch env.Sub {
	case "clean", "":
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
		return cachecmd.CacheVerifyCommand(env.Ctx, env.WsRoot, []string{"lint", "test", "build"},
			env.OutputFormat, newCacheVerifyRunner(env.Cfg, env.Global, engine.New().Run))
	default:
		return usageErrorf("unknown subcommand: cache %s\n  Available: clean, gc, verify", env.Sub)
	}
}

// newCacheVerifyRunner is the CLI-owned adapter from the cache verifier onto
// Engine.Run. The checker observes the real plan, keys, scheduler, capture, and
// restore paths without becoming a second execution composition.
func newCacheVerifyRunner(
	cfg *wsproto.Config,
	global GlobalFlags,
	run changePlanEngineRun,
) cachecmd.CacheVerifyRunner {
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
		if flags.Projects == "" && !flags.Impacted && !flags.All && flags.FilterTag == "" {
			flags.All = true
		}
		var hooks *wsproto.HooksConfig
		if cfg != nil {
			hooks = cfg.Hooks
		}
		request := engine.Request{
			WorkspaceRoot:    workspaceRoot,
			Config:           cfg,
			Commands:         []string{"lint", "test", "build"},
			Global:           flags,
			Hooks:            hooks,
			Preflight:        doctor.DoctorPreflight,
			EphemeralSession: true,
			Stdout:           io.Discard,
		}
		request.CacheVerification = &engine.CacheVerificationRequest{StoreRoot: storeRoot}
		result, err := run(ctx, request, output.NewPassthroughRenderer())
		return cachecmd.CacheVerifyEngineResult{
			ExitCode: result.ExitCode,
			Plan:     result.Plan,
			Projects: result.Projects,
			Results:  result.Results,
		}, err
	}
}

func cmdInfra(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	switch env.Sub {
	case "plan", "":
		if env.OutputFormat == "jsonl" {
			return lifecycle.InfraPlanJSON(env.WsRoot, env.Args)
		}
		return lifecycle.InfraPlan(env.WsRoot, env.Args)
	default:
		return usageErrorf("unknown subcommand: infra %s\n  Available: plan", env.Sub)
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
			Args:         env.Args,
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
	case "agent-content":
		return cmdMigrateAgentContent(env)
	default:
		return usageErrorf("unknown migration source: %s\n  Available: vnext, agent-content", env.Sub)
	}
}

// cmdMigrateAgentContent binds `migrate agent-content`'s three modes. They are
// mutually exclusive, because they differ in whether and in which direction the
// workspace is written; none means --check, the read-only mode.
func cmdMigrateAgentContent(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	parsed, err := parseCatalogCommandFlags("migrate agent-content", env.Args)
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 1 {
		return usageErrorf("migrate agent-content takes exactly one extension name, the extension whose agent content receives the superseded artifacts")
	}
	mode := agentctx.AgentContentMigrationCheck
	selected := 0
	for _, candidate := range []struct{ flag, mode string }{
		{"--check", agentctx.AgentContentMigrationCheck},
		{"--apply", agentctx.AgentContentMigrationApply},
		{"--rollback", agentctx.AgentContentMigrationRollback},
	} {
		if parsed.has(candidate.flag) {
			mode = candidate.mode
			selected++
		}
	}
	if selected > 1 {
		return usageErrorf("migrate agent-content: --check, --apply and --rollback are mutually exclusive")
	}
	return agentctx.MigrateAgentContent(env.Ctx, env.WsRoot, env.Cfg, parsed.positionals[0], mode, env.OutputFormat)
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
//
// `tree verify` checks local workflow evidence against that tree. Its stdout
// is always one verdict document, whatever --output asks, and a not-verified
// verdict exits 1 with nothing on stderr: the verdict already says why.
func cmdTree(env *CommandEnv) error {
	if env.Sub == "verify" {
		return cmdTreeVerify(env)
	}
	if env.Sub != "fingerprint" {
		return usageErrorf("unknown subcommand: tree %s\n  Available: fingerprint, verify", env.Sub)
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

// cmdTreeVerify runs the verifier on the flags after `tree verify`. The
// verifier parses them itself, so a missing or conflicting flag is a
// not-verified verdict on stdout rather than a usage error on stderr.
func cmdTreeVerify(env *CommandEnv) error {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve the working directory: %w", err)
	}
	err = treecmd.Verifier{Dir: workingDirectory, Stdout: iox.Stdout()}.Run(env.Args)
	if errors.Is(err, treecmd.ErrNotVerified) {
		return reportedFailure{err: err}
	}
	return err
}

// cmdCompose serves a workload together with the transitive closure of the
// workloads it runs with (internal/compose). Exactly one positional names the
// target, by project id or name; the parser leaves it in env.Args because
// compose is a positional leaf (commandmeta.PositionalLeaf).
func cmdCompose(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	parsed, err := parseCatalogCommandFlags("compose", env.Args)
	if err != nil {
		return err
	}
	selectors := parsed.positionals
	if len(selectors) != 1 {
		return usageErrorf("compose takes exactly one project: putnami compose <project> [--port <n>] [--no-watch] [--ready-timeout <duration>]")
	}
	return composecmd.Run(composecmd.Request{
		Ctx:           env.Ctx,
		WorkspaceRoot: env.WsRoot,
		Config:        env.Cfg,
		Selector:      selectors[0],
		Port:          parsed.value("--port"),
		NoWatch:       parsed.has("--no-watch"),
		ReadyTimeout:  parsed.value("--ready-timeout"),
		OutputFormat:  env.OutputFormat,
		Verbose:       env.Global.Verbose || env.Global.Debug,
		Quiet:         env.Global.Quiet,
		NoColor:       env.Global.Color != nil && !*env.Global.Color,
	})
}

// cmdQualify runs one workload's derived smoke contract against a target. The
// project is its single positional; the parser leaves it in env.Args because
// qualify is a positional leaf (commandmeta.PositionalLeaf).
func cmdQualify(env *CommandEnv) error {
	if err := env.requireWorkspace(); err != nil {
		return err
	}
	parsed, err := parseCatalogCommandFlags("qualify", env.Args)
	if err != nil {
		return err
	}
	if len(parsed.positionals) != 1 {
		return usageErrorf("qualify takes exactly one project: putnami qualify <project> --target <local|url>")
	}
	readyTimeout, err := parsePositiveDuration("--ready-timeout", parsed.value("--ready-timeout"))
	if err != nil {
		return err
	}
	requestTimeout, err := parsePositiveDuration("--request-timeout", parsed.value("--request-timeout"))
	if err != nil {
		return err
	}
	return qualifycmd.Run(env.Ctx, qualifycmd.Options{
		WorkspaceRoot:  env.WsRoot,
		Selector:       parsed.positionals[0],
		Target:         parsed.value("--target"),
		ExpectSHA:      parsed.value("--expect-sha"),
		PlatformPrefix: parsed.value("--platform-prefix"),
		ReadyTimeout:   readyTimeout,
		RequestTimeout: requestTimeout,
		PrintContract:  parsed.has("--print-contract"),
		OutputFormat:   env.OutputFormat,
		Config:         env.Cfg,
		Verbose:        env.Global.Verbose || env.Global.Debug,
	})
}

// parsePositiveDuration reads a Go duration flag; empty means the default (0).
func parsePositiveDuration(flag, raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, usageErrorf("%s must be a positive Go duration such as 30s or 2m", flag)
	}
	return value, nil
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

func cmdCompletion(env *CommandEnv) error {
	shell := env.Sub
	if shell == "" && len(env.Args) > 0 {
		shell = env.Args[0]
	}
	switch shell {
	case "bash":
		completion.CompletionBash(os.Stdout, env.WsRoot, env.Cfg)
	case "zsh":
		completion.CompletionZsh(os.Stdout, env.WsRoot, env.Cfg)
	case "fish":
		completion.CompletionFish(os.Stdout, env.WsRoot, env.Cfg)
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
			PrintHelpMan()
			return nil
		case "--markdown":
			PrintHelpMarkdown()
			return nil
		}
	}
	if env.Sub != "" {
		nestedSub := ""
		if len(env.Args) > 0 && !strings.HasPrefix(env.Args[0], "-") {
			nestedSub = env.Args[0]
		}
		command := resolveAliasOrSelf(env.Sub, env.Cfg.Aliases)
		if err := refuseHelpOfRemovedCoreRoot(env, command); err != nil {
			return err
		}
		if isStructuredCommand(command) || nestedSub != "" {
			PrintSubcommandHelp(command, nestedSub)
		} else {
			printJobCommandHelp(command, env.WsRoot, env.Cfg, nil)
		}
	} else {
		PrintHelp()
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

func cmdDev(env *CommandEnv) error {
	kind := env.Sub // "extension" or "template"
	action := ""
	args := env.Args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}
	switch kind {
	case "extension":
		switch action {
		case "validate":
			return extensions.ExtensionsValidate(args)
		default:
			return usageErrorf("unknown action: dev extension %s\n  Available: validate", action)
		}
	case "template":
		switch action {
		case "validate":
			return extensions.TemplatesValidate(args)
		case "test":
			return extensions.TemplatesTest(env.Ctx, args)
		case "package":
			return extensions.TemplatesPackage(env.Ctx, args)
		default:
			return usageErrorf("unknown action: dev template %s\n  Available: validate, test, package", action)
		}
	default:
		return usageErrorf("unknown subcommand: dev %s\n  Available: extension, template", kind)
	}
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
