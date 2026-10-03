package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	extensionscmd "go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
	"go.putnami.dev/tooling/cli/internal/sessionreporter"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/telemetry"
	"go.putnami.dev/tooling/cli/internal/useragent"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// stderrIsTTY is replaceable in tests so the first-run telemetry notice can be
// exercised without depending on the test runner's file descriptors.
var stderrIsTTY = output.StderrIsTTY

// Exit codes. The taxonomy is owned by go.putnami.dev/protocol/cli — the
// single source of truth shared with the extension SDK and cloud's cli-core.
// These package-local names alias the canonical constants so the many existing
// call sites need not change:
//
//	ExitError          → Failure (1): a job failed, or an unexpected error.
//	ExitUsage          → Usage   (2): bad invocation, no match, validation.
//	ExitAuth           → Auth    (3): authentication/authorization failed.
//	ExitAPI            → API      (4): a remote/upstream API call failed.
//	ExitSignalReceived → Signal (130): interrupted by a signal.
const (
	ExitSuccess        = protocolcli.ExitSuccess
	ExitError          = protocolcli.ExitFailure
	ExitUsage          = protocolcli.ExitUsage
	ExitAuth           = protocolcli.ExitAuth
	ExitAPI            = protocolcli.ExitAPI
	ExitSignalReceived = protocolcli.ExitSignal
)

// App is the root CLI application.
type App struct{}

// NewApp creates a new App instance.
func NewApp() (*App, error) {
	useragent.SetVersion(Version)
	telemetry.SetCLIVersion(Version)
	return &App{}, nil
}

// registerArtifactRoot registers the workspace as a root of the machine-global
// artifact store, so its GC never evicts the CLI and extensions this workspace
// links to, and keeps the running binary warm. Best-effort: a failure only costs
// that protection.
func registerArtifactRoot(wsRoot string) {
	if wsRoot == "" {
		return
	}
	artifacts := artifactstore.New(store.ResolveArtifactStoreRoot(wsRoot))
	_ = artifacts.RegisterWorkspace(wsRoot)
	if self, err := os.Executable(); err == nil {
		artifacts.TouchExecutable(self)
	}
}

// Run executes the CLI with the given arguments.
func (a *App) Run(ctx context.Context, args []string) int {
	// The release-set coordinator invokes the already-authoritative executable,
	// so its private child transport is captured before even considering another
	// pin relaunch. This removes the bearer and marker before any workspace-owned
	// setup or subprocess can observe them.
	ctx, providerChild := captureEarlyReleaseSetProviderCapability(ctx)

	if err := enterProcess(args); err != nil {
		printCommandError(os.Stderr, err)
		return ExitUsage
	}

	// Load config early for alias resolution
	cwd, _ := os.Getwd()
	wsRoot, _ := workspace.FindRoot(cwd)

	// A package.json root gives way to the user scope for a command group only
	// the user scope provides. The decision reads files and spawns nothing, so
	// it comes before the relaunch: a root the user scope takes over pins no CLI.
	claim := claimPackageRoot(ctx, args, cwd, wsRoot, providerChild)
	wsRoot = claim.root(wsRoot)

	// A hosted run, or one that enables the install provider, downloads the
	// pinned CLI and the lock-pinned extensions through the credential provider
	// of the user scope. The workspace's own provider is installed after the
	// parse, too late for those downloads. The bootstrap provider serves them
	// only, and ends before the relaunch execs and before the workspace's
	// provider is installed.
	boot := openBootstrapProvider(ctx, args, wsRoot, providerChild, os.Stderr)
	defer boot.close()

	// Re-exec into the workspace-pinned CLI when the running binary isn't it, so
	// putnami, putnamiw, and the MCP server all run one engine. On a successful
	// relaunch under Unix this call does not return.
	//
	// A pin is fail-closed: when the workspace pins a CLI this process could not
	// become, the run stops here rather than silently proceeding as a different
	// engine. The error names the recovery command, and is printed in human form
	// on stderr because it precedes output-mode resolution — nothing downstream,
	// including the parse, has run yet.
	//
	// A SOURCE WORKSPACE — one whose lock records `cli.source: "workspace"` —
	// is fail-closed too, for the mirror reason: it builds its own
	// engine, so there is no published binary to relaunch into and the only
	// acceptable binary is one built from that tree. `./putnamiw` proves that with
	// PUTNAMI_FROM_SOURCE plus PUTNAMI_FROM_SOURCE_KEY, the session's pin on the
	// content-keyed store blob it selected; anything else stops here with a hard
	// error naming `./putnamiw <this command>`. A Go test binary is accepted,
	// because `go test` produced it from that same tree.
	//
	// CLI-management invocations (`pin`, `version list/use`, `upgrade
	// --from-source`) are exempt: they must run as the invoked binary so the
	// pinned engine can be changed or self-hosted from source even when it is
	// broken or predates the command/flag. That exemption is the way out of
	// every failure below; PUTNAMI_NO_RELAUNCH is the additional way out of a
	// broken PUBLISHED pin, and deliberately does not apply to a source
	// workspace.
	if !providerChild && !launch.IsExemptInvocation(args) {
		if err := launch.Relaunch(ctx, wsRoot, args, boot.launchBootstrap()); err != nil {
			printCommandError(os.Stderr, err)
			return exitCodeForError(err)
		}
	}
	registerArtifactRoot(wsRoot)
	// Capture capability transports only after a possible pinned-CLI relaunch:
	// exec must carry them into the authoritative process. From this point on the
	// process environment is clean before artifact setup, extension discovery,
	// bootstrap, hooks, probes, cache providers, or repository jobs can spawn.
	if !providerChild {
		ctx = jobs.CaptureProcessCapabilities(ctx)
	}
	// The credential-provider choice holds for this process; nothing it
	// starts inherits the variable.
	providersEnv := takeProvidersEnv()
	ctx = sessionreporter.Capture(ctx)
	providerMode, err := jobs.ValidateReleaseSetProviderInvocation(ctx, args)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return ExitUsage
	}
	launchedFromPin := launch.ConsumeLaunched()

	cfg := wsproto.Load(wsRoot)

	// A runner provider launched this process to execute a bound portable
	// request. The typed request is the only invocation authority: it
	// is consumed here, after the pinned-CLI relaunch and before any argv parse.
	if requestPath := os.Getenv(runnerprovider.BoundRequestEnv); requestPath != "" {
		return a.runBoundRequest(ctx, wsRoot, cfg, args, requestPath, providersEnv, boot)
	}

	// A user-scope pin that did not load leaves the claim open until it is
	// repaired here, after the capability capture: a download spawns the
	// registry credential helper, which must not inherit a capability transport.
	if claim.ownsAfterRepair(ctx, os.Stderr) {
		wsRoot, cfg = "", wsproto.Load("")
	}

	// The bootstrap provider serves the lock-pinned extension downloads that
	// discovery below, or a structured command's install, would start, and ends
	// before the workspace's provider is installed.
	boot.ensureArtifactsAndClose(ctx, wsRoot, cfg, bootstrapEnsuresArtifacts(wsRoot, providerMode))

	// Discover extensions when the first argument might be an extension-owned
	// command group (e.g. "putnami cloud login"). Skip discovery for help/
	// version/flag-only invocations and for built-in commands so simple paths
	// stay fast.
	extensions, extensionGroups := discoverDispatchExtensions(ctx, args, wsRoot, cfg, providerMode)

	aliases := aliasesForProcessMode(cfg, providerMode)
	parsed := ParseArgs(args, aliases, extensionGroups)
	if providerMode && !extensionGroups[parsed.Commands[0]] {
		iox.Fprintf(os.Stderr, "putnami: internal release-set provider command group is unavailable\n")
		return ExitUsage
	}

	if parsed.Global.Version {
		mode, err := protocolcli.ResolveOutputMode(parsed.Global.Output, parsed.Global.JSON)
		if err != nil {
			iox.Fprintf(os.Stderr, "putnami: %v\n", err)
			return ExitUsage
		}
		if mode.IsStructured() {
			result := protocolcli.NewResultV2("version", map[string]any{
				"version":                  Version,
				"launchedFromWorkspacePin": launchedFromPin,
			}, nil)
			code, writeErr := protocolcli.WriteResultV2(os.Stdout, mode, result)
			if writeErr != nil {
				iox.Fprintf(os.Stderr, "putnami: write version result: %v\n", writeErr)
				return ExitError
			}
			return code
		}
		if launchedFromPin {
			iox.Fprintf(os.Stdout, "putnami %s (launched from workspace pin)\n", Version)
		} else {
			iox.Fprintf(os.Stdout, "putnami %s\n", Version)
		}
		return ExitSuccess
	}

	if parsed.Global.Help {
		if parsed.Global.HelpMan {
			PrintHelpMan()
			return ExitSuccess
		}
		if parsed.Global.HelpMD {
			PrintHelpMarkdown()
			return ExitSuccess
		}
		if len(parsed.Commands) > 0 && parsed.Commands[0] != "help" {
			cmd := parsed.Commands[0]
			if extensionGroups[cmd] {
				printCommandGroupHelp(ctx, os.Stdout, wsRoot, cfg, extensions, cmd, parsed.Subcommand, parsed.Global.Output)
			} else if isStructuredCommand(cmd) {
				PrintSubcommandHelp(cmd, parsed.Subcommand)
			} else {
				if code := printJobCommandHelpForWorkspace(cmd, wsRoot, cfg, extensions); code != ExitSuccess {
					return code
				}
			}
		} else {
			PrintHelp()
		}
		return ExitSuccess
	}

	// The single parse-and-validation pass reports here, after
	// the help and version shortcuts so both keep working on a malformed
	// invocation. A usage error is exit 2 with the message on stderr; the
	// deprecation notices ride along on stderr and never block the run.
	if parsed.Err != nil {
		printCommandError(os.Stderr, parsed.Err)
		return exitCodeForError(parsed.Err)
	}
	if len(parsed.Commands) == 0 {
		PrintHelp()
		return ExitSuccess
	}
	printParseWarnings(os.Stderr, parsed.Global, parsed.Warnings)

	// PUTNAMI_MACHINE_OUTPUT=v1 no longer selects anything: an earlier change deleted
	// the v1 emitters, so a run still carrying the canary's rollback lever
	// silently receives v2 documents. Say so ONCE on stderr rather than let a
	// consumer discover it by parsing an unexpected shape — and only warn, never
	// fail, because the variable was a documented lever and a CI runner that
	// still exports it must keep working. Stderr, and after the parse, so it
	// cannot corrupt a machine stdout stream.
	machine.WarnRetiredSelection(os.Stderr, os.Getenv(machine.RetiredSelectionEnv))

	// Resolve --output / --json into a single canonical output mode, validating
	// the value and rejecting a --json/--output conflict. Done once here so every
	// downstream path (structured, job, extension) sees the resolved value.
	resolvedOutput, err := protocolcli.ResolveOutputMode(parsed.Global.Output, parsed.Global.JSON)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return ExitUsage
	}
	parsed.Global.Output = string(resolvedOutput)

	// Resolve the deployment profile (--profile flag > PUTNAMI_PROFILE > workspace
	// config > dev) once here, before dispatch, so every downstream path — job
	// commands, structured commands, and extension commands — reads the same
	// value off parsed.Global. A value outside dev|test|production is a hard usage
	// error (it catches stale `--profile <trace-path>` calls, now --trace-profile).
	if err := resolveProfile(&parsed.Global, cfg); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return ExitUsage
	}
	// Resolve cache authority once, before hooks or bootstrap can mutate state.
	// Extension-owned command groups defer this until their flat job command is
	// known; built-in structured commands do not use the remote job cache.
	if err := resolveCommandCacheTrust(&parsed.Global, cfg, parsed.Commands, parsed.Subcommand, extensionGroups); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return ExitUsage
	}
	// A run that may publish reads its bound commit's ancestry now, before the
	// provider starts and before the bootstrap or an install runs repository
	// code that could rewrite refs; its engine run reuses that snapshot
	// (engine.CaptureAncestry).
	ctx = engine.CaptureAncestry(ctx, wsRoot, parsed.Commands, nil)
	// Resolve --cpu-policy and --providers, and start the credential provider
	// for the purposes the invocation enabled. From here on, bootstrap,
	// installs, extension commands and jobs download through it. Downloads
	// before the parse (lock materialization for dispatch) keep their
	// host-keyed credential, because the flag is not known yet. On a hosted
	// run they ask no host-keyed credential: the user scope's provider serves
	// them or they go out without one (extension.AuthorizeRegistryRequest).
	stopProviders, code := resolveExecutionPolicy(&parsed.Global, cfg, parsed.Commands, wsRoot, extensions, providersEnv)
	if code != ExitSuccess {
		return code
	}
	defer stopProviders()
	// Analysis-oriented reads need the exact locked extension manifests, tools
	// and documentation even in a fresh worktree. This narrow preparation never
	// invokes the full install lifecycle; failures degrade to the core local
	// answer and are surfaced by MCP guidance when applicable.
	ctx = withInvocationReadPreparation(ctx, wsRoot, cfg, parsed)

	// First-use workspace bootstrap: a fresh checkout, a new worktree, or
	// a CI runner may have no installed workspace state yet, so the first
	// arbitrary command (build, test, or an extension command like "cloud ...")
	// would run before extension-local state (dependencies, Cloud link/cache,
	// registry token recipes) is restored. Lazily run `putnami install` when the
	// recorded install state is missing or stale. Built-in structured commands
	// (install, deps, ...) are excluded — they restore workspace state on their
	// own — and the guard is inert under --plan/--dry-run and best-effort.
	bootstrapGlobal := parsed.Global
	engine.ApplyEnvOverrides(&bootstrapGlobal, cfg)
	// A hosted job command reads the remote cache through one provider, and
	// hands its reporters the run credential, which the bootstrap starts
	// before the implicit install's first repository code.
	hostedCache, hostedReporters := &engine.HostedRemoteCache{}, &engine.HostedReporters{}
	defer closeHostedHolders(hostedCache, hostedReporters)
	if !providerMode && wsRoot != "" && !isStructuredCommand(parsed.Commands[0]) {
		lifecycle.EnsureWorkspaceBootstrap(ctx, wsRoot, cfg, lifecycle.BootstrapOptions{
			Command:    parsed.Commands[0],
			Subcommand: parsed.Subcommand,
			Output:     bootstrapGlobal.Output,
			Display:    bootstrapLifecycleDisplay(bootstrapGlobal),
			// The implicit install's workspace-install pass runs through the same
			// engine adapter every explicit lifecycle command uses.
			// Bootstrap picks its OWN human writer from Output — stderr under a
			// structured mode — so it takes the runner alone, not a lifecycleEnv.
			RunJob:               RunWorkspaceJob,
			Plan:                 bootstrapGlobal.Plan,
			DryRun:               bootstrapGlobal.DryRun,
			BeforeRepositoryCode: startHostedHoldersOfJobCommand(hostedCache, hostedReporters, parsed, extensionGroups, wsRoot, cfg),
		})
	}

	// Extension-owned structured commands (e.g. "cloud login") are routed
	// before built-in structured commands so an extension can never shadow a
	// built-in by accident, but extension dispatch is checked first only when
	// the command name actually matches an extension group.
	if extensionGroups[parsed.Commands[0]] {
		return a.runExtensionStructuredCommand(
			ctx, parsed, cfg, wsRoot, extensions, os.Stdin, os.Stdout, os.Stderr,
		)
	}

	// Handle built-in structured commands
	if isStructuredCommand(parsed.Commands[0]) {
		return a.runStructuredCommand(ctx, parsed, cfg, wsRoot)
	}

	// Job commands require a workspace
	if wsRoot == "" {
		return noWorkspaceFound(os.Stderr)
	}

	return a.runTerminalSession(ctx, cfg, parsed, wsRoot, hostedCache, hostedReporters)
}

// ErrHostedExtensionCommand refuses an extension command group on a hosted
// run (runcredential.Flag), before anything starts. A hosted run supports job
// commands, such as build, and the built-in commands, such as install.
var ErrHostedExtensionCommand = fmt.Errorf("an extension command group does not accept %s: "+
	"it resolves its remote cache after the workspace install ran repository code, and a hosted run starts its cache provider before that; "+
	"run a job command, such as build, or a built-in command, such as install", runcredential.Flag)

// closeHostedHolders closes the reporters, then the remote cache provider,
// that a hosted job command started and its run did not adopt.
func closeHostedHolders(cache *engine.HostedRemoteCache, reporters *engine.HostedReporters) {
	reporters.Close()
	cache.Close()
}

// startHostedHoldersOfJobCommand is the first-use bootstrap's
// BeforeRepositoryCode for a job command: it starts the provider of the remote
// cache the command's run then reads (engine.Request.HostedRemoteCache), then
// the reporters that run's session adopts (engine.Request.HostedReporters),
// with the flags that run has. An extension command group resolves its cache
// trust only once its job command is known, and its run starts its own
// provider, so its bootstrap starts none; a hosted run refuses it before the
// bootstrap (ErrHostedExtensionCommand).
func startHostedHoldersOfJobCommand(hosted *engine.HostedRemoteCache, reporters *engine.HostedReporters, parsed *ParsedArgs, extensionGroups map[string]bool, wsRoot string, cfg *wsproto.Config) func(context.Context) error {
	if extensionGroups[parsed.Commands[0]] {
		return nil
	}
	return func(ctx context.Context) error {
		req := &engine.Request{WorkspaceRoot: wsRoot, Config: cfg, Global: parsed.Global}
		if _, err := hosted.Start(ctx, req); err != nil {
			return err
		}
		reporters.Start(ctx, req)
		return nil
	}
}

func bootstrapLifecycleDisplay(global GlobalFlags) lifecycle.LifecycleDisplay {
	colorEnabled := global.Color == nil || *global.Color
	return lifecycle.LifecycleDisplay{
		Verbose:     global.Verbose,
		Debug:       global.Debug,
		Quiet:       global.Quiet,
		NoColor:     !colorEnabled,
		Interactive: colorEnabled && !output.StructuredOutput(global.Output) && output.ShouldUseLiveRenderer(os.Stdout),
	}
}

func automaticReadPreparationCommand(command, subcommand string) bool {
	switch command {
	case "workspace":
		return subcommand == "" || subcommand == "describe"
	case "projects":
		return subcommand == "" || subcommand == "list" || subcommand == "describe"
	case "extensions":
		return subcommand == "list"
	case "context":
		return subcommand == "map" || subcommand == "pack"
	}
	return false
}

// withInvocationReadPreparation applies withAutomaticReadPreparation to one
// parsed invocation. `extensions … --user` is exempt: it reads the user scope,
// never the workspace it happens to run in.
func withInvocationReadPreparation(ctx context.Context, wsRoot string, cfg *wsproto.Config, parsed *ParsedArgs) context.Context {
	if parsed.Commands[0] == "extensions" && extensionscmd.HasUserScopeFlag(parsed.RawJobArgs) {
		return ctx
	}
	return withAutomaticReadPreparation(ctx, wsRoot, cfg, parsed.Commands[0], parsed.Subcommand)
}

func withAutomaticReadPreparation(ctx context.Context, wsRoot string, cfg *wsproto.Config, command, subcommand string) context.Context {
	if wsRoot == "" || !automaticReadPreparationCommand(command, subcommand) {
		return ctx
	}
	report := lifecycle.PrepareReadOnly(ctx, wsRoot, cfg)
	return lifecycle.WithReadPreparation(ctx, report)
}

// resolveCommandCacheTrust resolves the remote cache authority of a job
// command. A built-in structured command reads no remote job cache, and an
// extension command group resolves it once its job command is known, after
// the first-use bootstrap. A hosted run starts its remote cache provider
// before the bootstrap's repository code, so it refuses an extension command
// group here (ErrHostedExtensionCommand).
func resolveCommandCacheTrust(global *GlobalFlags, cfg *wsproto.Config, commands []string, subcommand string, extensionGroups map[string]bool) error {
	if extensionGroups[commands[0]] && runcredential.Hosted() {
		return cmderr.Classify(fmt.Errorf("putnami %s: %w", commands[0], ErrHostedExtensionCommand), cmderr.ErrUsage)
	}
	if isStructuredCommand(commands[0]) || extensionGroups[commands[0]] {
		return nil
	}
	return resolveCacheTrust(global, cfg, commands, subcommand)
}

// enterProcess takes what the invocation hands this process before anything
// else runs. The run credential is read, and its descriptor closed, before
// anything can start a process or replace this one: the relaunch hands it to
// the pinned CLI on a fresh descriptor, and nothing else inherits it. An agent
// host can then supply its active session directory independently from the
// process cwd; it is entered before root discovery and pin relaunch so even an
// older pinned CLI inherits the correct worktree after exec.
//
// A CLI started inside the workspace-fetch of a hosted run runs beside the job
// credential, so it refuses first, before it reads a workspace, discovers an
// extension or starts anything (runcredential.HostedFetchEnv).
func enterProcess(args []string) error {
	if err := runcredential.RefuseInHostedFetch(os.Getenv); err != nil {
		return err
	}
	if err := runcredential.Capture(args); err != nil {
		return err
	}
	if err := requireHostedArtifactStore(runcredential.Hosted(), store.GlobalArtifactRoot); err != nil {
		return err
	}
	return launch.EnterAgentWorkspace(args)
}

// ErrHostedArtifactStore refuses a hosted run whose artifact store would fall
// back to the workspace: with no $HOME and no PUTNAMI_ARTIFACT_DIR, the store
// is <workspace>/.putnami/artifacts, where the repository can commit the
// pinned CLI or an extension runtime that the run then trusts without a
// download.
var ErrHostedArtifactStore = fmt.Errorf("a hosted run (%s) needs an artifact store outside the workspace: "+
	"set HOME or PUTNAMI_ARTIFACT_DIR", runcredential.Flag)

// requireHostedArtifactStore refuses a hosted run whose artifact store
// resolves inside the workspace (ErrHostedArtifactStore). globalRoot is
// store.GlobalArtifactRoot, which reports false when the store would fall
// back to the workspace.
func requireHostedArtifactStore(hosted bool, globalRoot func() (string, bool)) error {
	if !hosted {
		return nil
	}
	if _, ok := globalRoot(); !ok {
		return cmderr.Classify(ErrHostedArtifactStore, cmderr.ErrUsage)
	}
	return nil
}

func captureEarlyReleaseSetProviderCapability(ctx context.Context) (context.Context, bool) {
	providerChild := jobs.HasInternalReleaseSetProviderCapability()
	if providerChild {
		ctx = jobs.CaptureProcessCapabilities(ctx)
	}
	return ctx, providerChild
}

func ensureArtifactsForProcessMode(
	ctx context.Context,
	wsRoot string,
	cfg *wsproto.Config,
	providerMode bool,
) {
	if providerMode {
		return
	}
	_ = lifecycle.EnsureArtifacts(ctx, wsRoot, cfg)
}

func aliasesForProcessMode(cfg *wsproto.Config, providerMode bool) map[string]string {
	if providerMode {
		// The coordinator owns the exact protocol argv. Repository aliases must
		// not redirect `cloud` to another structured group or flat command.
		return nil
	}
	return cfg.Aliases
}

// shouldDiscoverExtensions reports whether App.Run should pre-load extensions
// to identify extension-owned command groups. Discovery is skipped for
// flag-only invocations (--help, --version), built-in structured commands,
// and when no workspace is present.
func shouldDiscoverExtensions(args []string, wsRoot string) bool {
	if wsRoot == "" {
		return false
	}
	if len(args) == 0 {
		return false
	}
	first := args[0]
	if first == "" || strings.HasPrefix(first, "-") {
		return false
	}
	// Built-in structured commands and aliases never compete with extension
	// groups, so we skip discovery for them. Flat job commands (build, test,
	// etc.) still trigger discovery — the cost is paid once per CLI run and
	// the extension list is needed by the job pipeline anyway.
	if isStructuredCommand(first) {
		return false
	}
	return true
}

// discoverDispatchExtensions loads the extensions whose command groups App.Run
// dispatches: the workspace's inside a workspace, the user scope's outside one,
// and none for an invocation that cannot name an extension command group.
func discoverDispatchExtensions(
	ctx context.Context,
	args []string,
	wsRoot string,
	cfg *wsproto.Config,
	providerMode bool,
) ([]*extension.ExtensionDescription, map[string]bool) {
	if shouldDiscoverExtensions(args, wsRoot) {
		// Materialize declared extensions from the lock before discovery so a
		// fresh git worktree resolves them with no explicit `putnami install`
		// (zero-init). Best-effort: a genuinely un-installable extension is
		// surfaced by the missing-extension guard in the run plan, not here.
		ensureArtifactsForProcessMode(ctx, wsRoot, cfg, providerMode)
		extensions, _ := extension.DiscoverExtensions(wsRoot, cfg, projectPathsForRoot(wsRoot))
		return extensions, commandGroupsFor(extensions, wsRoot, providerMode)
	}
	if !providerMode && shouldDiscoverUserScope(args, wsRoot) {
		// Outside any workspace the only extensions are the ones pinned in the
		// user scope. Their command groups dispatch like a workspace's, and only
		// a subcommand declared `workspace: optional` runs; a failure here is a
		// warning, so a built-in never depends on the user scope.
		extensions := discoverUserScopeExtensions(ctx, os.Stderr)
		return extensions, extension.CommandGroupNames(extensions)
	}
	return nil, nil
}

// shouldDiscoverUserScope reports whether App.Run should load the user scope:
// outside any workspace, for a first argument that could name an extension
// command group. Flag-only invocations and built-in structured commands
// (`--version`, `init`, `upgrade`, `help`, `completion`, …) never read it.
func shouldDiscoverUserScope(args []string, wsRoot string) bool {
	if wsRoot != "" || len(args) == 0 {
		return false
	}
	first := args[0]
	if first == "" || strings.HasPrefix(first, "-") {
		return false
	}
	return !isStructuredCommand(first)
}

// packageRootClaim is the user scope's claim on an invocation whose only root
// is a package.json with a "workspaces" field. The user scope serves every
// directory without putnami.workspace.json, and an npm or yarn monorepo has
// none: a command group the user scope provides runs there as outside any
// workspace, and nothing is bootstrapped or written into the repository.
type packageRootClaim struct {
	// owns is set when a loaded user-scope pin provides the command group and
	// the package root does not declare that extension.
	owns bool
	// repair is set when a user-scope pin did not load, so the claim is decided
	// by ownsAfterRepair.
	repair   bool
	wsRoot   string
	userRoot string
	command  string
}

// claimPackageRoot decides the claim from files, before the pinned-CLI
// relaunch. A putnami.workspace.json at or above the working directory always
// wins, and so does the package root when its own extensions provide the
// command or its package.json declares the extension that provides it.
//
// A comma list, an alias to one, and an alias to a built-in command never name
// a group; the internal release-set provider and a bound runner request never
// read the user scope. For any other command the check reads one lock file
// when the user scope pins nothing. Otherwise it resolves aliases, discovers
// the package root's extensions, and loads the user-scope pins without
// repairing them.
func claimPackageRoot(ctx context.Context, args []string, cwd, wsRoot string, providerChild bool) packageRootClaim {
	if wsRoot == "" || providerChild || os.Getenv(runnerprovider.BoundRequestEnv) != "" ||
		!shouldDiscoverUserScope(args, "") || strings.Contains(args[0], ",") || workspaceManifestAbove(cwd) {
		return packageRootClaim{}
	}
	userRoot, err := extension.ResolveUserScopeRoot()
	if err != nil || !extension.UserScopeMayPin(userRoot) {
		return packageRootClaim{}
	}
	cfg := wsproto.Load(wsRoot)
	command := resolveAliasOrSelf(args[0], cfg.Aliases)
	if strings.Contains(command, ",") || isStructuredCommand(command) || packageRootProvides(wsRoot, cfg, command) {
		return packageRootClaim{}
	}
	claim := packageRootClaim{wsRoot: wsRoot, userRoot: userRoot, command: command}
	if result, err := extension.DiscoverUserScopeExtensions(ctx, userRoot, nil); err == nil {
		if owns, found := claim.provides(result); found || len(result.Skipped) == 0 {
			claim.owns = owns
			return claim
		}
	}
	claim.repair = true
	return claim
}

// root is the workspace root of the run: none when the user scope owns it.
func (c packageRootClaim) root(wsRoot string) string {
	if c.owns {
		return ""
	}
	return wsRoot
}

// ownsAfterRepair decides a claim a user-scope pin left open. It repairs the
// user scope as a run outside a workspace does. When the user scope still does
// not provide the command group, its warnings are printed and the package root
// keeps the run.
func (c packageRootClaim) ownsAfterRepair(ctx context.Context, stderr io.Writer) bool {
	if !c.repair {
		return false
	}
	var installer *extension.Installer
	if extension.ClaimUserScopeRepair(c.userRoot) {
		installer = extension.NewUserScopeInstaller(c.userRoot)
	}
	result, err := extension.DiscoverUserScopeExtensions(ctx, c.userRoot, installer)
	if err != nil {
		iox.Fprintf(stderr, "putnami: warning: %v\n", err)
		return false
	}
	if owns, found := c.provides(result); found {
		return owns
	}
	for _, skipped := range result.Skipped {
		iox.Fprintf(stderr, "putnami: warning: %v\n", skipped.Reason)
	}
	return false
}

// provides reports whether a user-scope extension in result provides the
// claimed command group (found), and whether the package root leaves that
// extension to the user scope (owns).
func (c packageRootClaim) provides(result *extension.DiscoveryResult) (owns, found bool) {
	for _, ext := range result.Extensions {
		if _, ok := ext.CommandGroups[c.command]; ok {
			return !extension.PackageRootDeclares(c.wsRoot, ext.Name), true
		}
	}
	return false, false
}

// packageRootProvides reports whether the extensions a package root discovers
// provide command, as a command group or as a flat command.
func packageRootProvides(wsRoot string, cfg *wsproto.Config, command string) bool {
	extensions, _ := extension.DiscoverExtensions(wsRoot, cfg, projectPathsForRoot(wsRoot))
	if commandGroupsFor(extensions, wsRoot, false)[command] {
		return true
	}
	for _, ext := range extensions {
		if _, ok := ext.Commands[command]; ok {
			return true
		}
	}
	return false
}

// workspaceManifestAbove reports whether dir or one of its parents holds
// putnami.workspace.json.
func workspaceManifestAbove(dir string) bool {
	for {
		if wsproto.IsWorkspaceConfig(dir) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// discoverUserScopeExtensions loads the extensions pinned in the user scope.
// The first process of a nested chain repairs missing links through the
// artifact store or a verified download; the processes it spawns only load.
// Every failure is reported on stderr and leaves the extension out, never
// failing the invocation.
func discoverUserScopeExtensions(ctx context.Context, stderr io.Writer) []*extension.ExtensionDescription {
	userRoot, err := extension.ResolveUserScopeRoot()
	if err != nil {
		return nil
	}
	var installer *extension.Installer
	if extension.ClaimUserScopeRepair(userRoot) {
		installer = extension.NewUserScopeInstaller(userRoot)
	}
	result, err := extension.DiscoverUserScopeExtensions(ctx, userRoot, installer)
	if err != nil {
		iox.Fprintf(stderr, "putnami: warning: %v\n", err)
		return nil
	}
	for _, skipped := range result.Skipped {
		iox.Fprintf(stderr, "putnami: warning: %v\n", skipped.Reason)
	}
	return result.Extensions
}

// noWorkspaceFound reports a command that needs a workspace and found none. It
// is the message of every job command and every extension subcommand that is
// not declared `workspace: optional`.
func noWorkspaceFound(stderr io.Writer) int {
	iox.Fprintf(stderr, "putnami: no workspace found (looking for %s)\n", wsproto.WorkspaceConfigFilename)
	return ExitError
}

// projectPathsForRoot returns the workspace-relative project paths used by
// extension discovery. Extracted for use by App.Run, which discovers
// extensions before runJobCommands has a chance to load the workspace.
func projectPathsForRoot(wsRoot string) []string {
	ws, err := workspace.Load(wsRoot)
	if err != nil || ws == nil {
		return nil
	}
	paths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		paths[i] = p.Path
	}
	return paths
}

// runTerminalSession owns the once-per-process telemetry session around a job
// command: the consent notice, the run gates, and the single session:end. It was
// named runWithConfigHooks until a rework moved the lifecycle hooks it
// used to bracket into Engine.Run, next to the version stamp that has to be read
// before a hook can dirty the tree.
func (a *App) runTerminalSession(ctx context.Context, cfg *wsproto.Config, parsed *ParsedArgs, wsRoot string, hostedCache *engine.HostedRemoteCache, hostedReporters *engine.HostedReporters) (exitCode int) {
	telemetryStartedAt := time.Now()
	tc := telemetry.NewClient()
	interactive := stderrIsTTY()
	if tc.PrepareRun(interactive) {
		iox.Fprintln(os.Stderr, telemetry.FirstRunNotice)
	}
	defer func() {
		// Lifecycle hooks may run `putnami telemetry off` after the client was
		// created. Re-read persisted consent so the stale client cannot restore
		// the deleted buffer with an end event.
		if !tc.CanCollect() {
			tc.Discard()
			return
		}
		tc.TrackSessionEnd(telemetry.SessionEnd{
			ExitCode:    exitCode,
			DurationMS:  time.Since(telemetryStartedAt).Milliseconds(),
			Interactive: interactive,
		})
		_ = tc.Flush()
	}()

	// The version stamp and the CLI/command lifecycle hooks that used to bracket
	// this call are engine stages now: the stamp has to be read before the
	// before-hooks can dirty the tree, so the two belong to the same
	// owner. What stays here is the once-per-process telemetry session above.
	return a.runJobCommands(ctx, parsed, cfg, wsRoot, tc, interactive, hostedCache, hostedReporters)
}
