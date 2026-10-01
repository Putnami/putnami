package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/completion"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	providerext "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hooks"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// restartUpgradeProcess is a seam for the post-update process replacement.
// launch.Restart does not return after a successful Unix exec, so tests replace
// it to assert the binary and remaining-phase arguments without replacing the
// test process.
var restartUpgradeProcess = launch.Restart

var versionUpdateWithOptions = versioncmd.VersionUpdateWithOptions

var refreshCompletionsAfterCLIUpdateFn = refreshCompletionsAfterCLIUpdate

type upgradeReleaseSetResolver interface {
	Resolve(context.Context, *distribution.ResolveRequest) (*distribution.ResolveResponse, error)
}

var newUpgradeReleaseSetResolver = func() (upgradeReleaseSetResolver, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate putnami executable: %w", err)
	}
	// The client starts `putnami cloud release-set`, a CLI that loads the
	// workspace's extensions.
	runcredential.MarkRepositoryCodeStarted("putnami cloud release-set")
	return releaseset.NewClient(executable), nil
}

var validateUpgradeReleaseSetProvider = func(wsRoot string, cfg *wsproto.Config) error {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace for release-set provider discovery: %w", err)
	}
	projectPaths := make([]string, len(ws.Projects))
	for index, project := range ws.Projects {
		projectPaths[index] = project.Path
	}
	discovered, err := providerext.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		return fmt.Errorf("discover release-set provider: %w", err)
	}
	provider, err := providerext.ResolveReservedProvider(discovered.Extensions, distribution.ProviderCommandName)
	if err != nil {
		return fmt.Errorf("resolve release-set provider: %w", err)
	}
	if provider == nil {
		detail := fmt.Sprintf("install exactly one extension declaring %q", distribution.ProviderCommandName)
		if cause := providerext.SkippedProviderCause(discovered.Skipped); cause != "" {
			detail += ". " + cause
		}
		return fmt.Errorf("%w: %s", releaseset.ErrProviderAbsent, detail)
	}
	return nil
}

// UpgradeFlags controls which upgrade phases to run.
type UpgradeFlags struct {
	CLI        bool
	Extensions bool
	Deps       bool
	Channel    string
	Release    string
	Version    string
	DryRun     bool
	// Namespace names the release-set owner for --release. An immutable id is
	// scoped by its namespace and nothing in the consuming workspace knows it,
	// so it is supplied rather than guessed.
	Namespace string
	// ContinueOnError lets the other ecosystems finish when one of them cannot
	// resolve the selector. Without it the first failure stops the run, so a
	// channel that npm serves and the Go origin does not moves neither.
	ContinueOnError bool
	// FromSource builds the CLI from the current workspace's source and
	// installs it as the active binary, bypassing the download channel. It is
	// CLI-only and mutually exclusive with the channel/release/version
	// selectors and the extensions/deps phases.
	FromSource bool
	// Providers are the invocation providers this process enabled, from
	// --providers or PUTNAMI_PROVIDERS. The restart into an updated CLI
	// carries them as PUTNAMI_PROVIDERS, never as --providers: the restart
	// target can be an older CLI, which refuses an unknown flag and ignores an
	// unknown variable.
	Providers []string
}

type upgradeSelector struct {
	Mode            string
	Display         string
	RegistryTarget  string
	DepsTarget      string
	Namespace       string
	ExplicitChannel bool
	ReleaseSetMode  bool
	// ReleaseSet is the resolved immutable snapshot --release names, carried as
	// the channel head shape every consumer already reads: {ref, generation,
	// releaseSet}. An immutable set is named by no channel, so its generation
	// is 0.
	ReleaseSet *distribution.ChannelHead
}

func resolveUpgradeSelector(flags UpgradeFlags) (upgradeSelector, error) {
	channel := strings.TrimSpace(flags.Channel)
	releaseID := strings.TrimSpace(flags.Release)
	version := strings.TrimSpace(flags.Version)

	namespace := strings.TrimSpace(flags.Namespace)

	explicit := 0
	if channel != "" {
		explicit++
	}
	if releaseID != "" {
		explicit++
	}
	if version != "" {
		explicit++
	}
	if explicit > 1 {
		return upgradeSelector{}, fmt.Errorf("--release, --channel, and --version are mutually exclusive")
	}
	if namespace != "" && releaseID == "" {
		return upgradeSelector{}, fmt.Errorf("--namespace scopes a release-set id and is accepted only with --release")
	}

	if releaseID != "" {
		if namespace == "" {
			return upgradeSelector{}, fmt.Errorf("--release needs --namespace: a release-set id is scoped by the namespace that published it")
		}
		return upgradeSelector{
			Mode:           "release",
			Display:        releaseID,
			DepsTarget:     releaseID,
			Namespace:      namespace,
			ReleaseSetMode: true,
		}, nil
	}

	if version != "" {
		return upgradeSelector{
			Mode:           "version",
			Display:        version,
			RegistryTarget: version,
			DepsTarget:     version,
		}, nil
	}

	if channel == "" {
		channel = "stable"
		return upgradeSelector{
			Mode:           "channel",
			Display:        channel,
			RegistryTarget: "latest",
			DepsTarget:     "latest",
		}, nil
	}
	registryTarget := channel
	depsTarget := channel
	if channel == "stable" {
		registryTarget = "latest"
		depsTarget = "latest"
	}
	// A channel is resolved through each ecosystem's own registry, so it needs
	// no release set and no namespace.
	return upgradeSelector{
		Mode:            "channel",
		Display:         channel,
		RegistryTarget:  registryTarget,
		DepsTarget:      depsTarget,
		ExplicitChannel: true,
	}, nil
}

// releaseSetRequest builds the resolve request for the one selector that still
// uses a release set. The namespace comes from --namespace, never from the
// workspace name: a consumer workspace is not the publisher, and deriving the
// namespace from its name made every consumer ask for a namespace the
// coordinator does not own.
func (selector upgradeSelector) releaseSetRequest() (*distribution.ResolveRequest, error) {
	if selector.Mode != "release" {
		return nil, fmt.Errorf("selector %q does not use a release set", selector.Mode)
	}
	request := &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       strings.TrimSpace(selector.Namespace),
		ReleaseID:       selector.Display,
	}
	if diagnostics := distribution.ValidateResolveRequest(request); diag.HasErrors(diagnostics) {
		return nil, fmt.Errorf("invalid upgrade release-set selector: %s", firstUpgradeReleaseSetDiagnostic(diagnostics))
	}
	return request, nil
}

// resolveUpgradeReleaseSet resolves the immutable snapshot --release names.
// Channels do not come through here at all: the channel name is passed to the
// language extensions, and each one reads it from its own registry (an npm
// dist-tag, a Go version query). That is what lets a workspace which is not the
// publisher upgrade at all — see ADR 0020.
func resolveUpgradeReleaseSet(ctx context.Context, wsRoot string, cfg *wsproto.Config, selector upgradeSelector) (upgradeSelector, error) {
	if !selector.ReleaseSetMode {
		return selector, nil
	}
	if cfg == nil {
		return upgradeSelector{}, fmt.Errorf("resolve upgrade release set: workspace config is required")
	}
	request, err := selector.releaseSetRequest()
	if err != nil {
		return upgradeSelector{}, err
	}
	if err := validateUpgradeReleaseSetProvider(wsRoot, cfg); err != nil {
		return upgradeSelector{}, err
	}
	resolver, err := newUpgradeReleaseSetResolver()
	if err != nil {
		return upgradeSelector{}, fmt.Errorf("prepare release-set provider: %w", err)
	}
	response, err := resolver.Resolve(ctx, request)
	if err != nil {
		return upgradeSelector{}, fmt.Errorf("resolve upgrade release set %q: %w", selector.Display, err)
	}
	if response == nil {
		return upgradeSelector{}, fmt.Errorf("resolve upgrade release set %q: provider returned no response", selector.Display)
	}
	// releaseset.Client already performs this validation. Keeping the exchange
	// check at the orchestration boundary also makes injected resolvers obey
	// the identical fail-closed contract.
	if diagnostics := distribution.ValidateResolveExchange(request, response); diag.HasErrors(diagnostics) {
		return upgradeSelector{}, fmt.Errorf("resolve upgrade release set %q: %s", selector.Display, firstUpgradeReleaseSetDiagnostic(diagnostics))
	}
	if response.Release == nil {
		return upgradeSelector{}, fmt.Errorf("resolve upgrade release set %q: provider answered no immutable release", selector.Display)
	}
	selector.ReleaseSet = response.Release
	return selector, nil
}

func firstUpgradeReleaseSetDiagnostic(diagnostics []diag.Diagnostic) string {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == diag.Error {
			return diagnostic.String()
		}
	}
	if len(diagnostics) > 0 {
		return diagnostics[0].String()
	}
	return "unknown release-set validation failure"
}

// ErrHostedUpgrade refuses `putnami upgrade` on a hosted run
// (runcredential.Flag). An upgrade rewrites the lock a hosted run executes as
// committed, and it restarts the CLI it installed after the deps-upgrade
// hooks, which are repository code: that process would receive the run
// credential after repository code ran.
var ErrHostedUpgrade = cmderr.Classify(
	fmt.Errorf("putnami upgrade does not accept %s: a hosted run executes the committed lock; upgrade without the run credential and commit the lock it writes",
		runcredential.Flag),
	cmderr.ErrUsage)

// Upgrade runs one or more upgrade phases: CLI, extensions, templates, and
// framework dependencies. When no flags are set, all phases run. A hosted run
// is refused before any phase starts (ErrHostedUpgrade).
func Upgrade(ctx context.Context, wsRoot string, cfg *wsproto.Config, currentVersion string, binDir string, flags UpgradeFlags, env LifecycleEnv) error {
	if runcredential.Hosted() {
		return ErrHostedUpgrade
	}
	return upgradePhases(ctx, wsRoot, cfg, currentVersion, binDir, flags, env)
}

// upgradePhases is Upgrade once the run is known not to be hosted.
func upgradePhases(ctx context.Context, wsRoot string, cfg *wsproto.Config, currentVersion string, binDir string, flags UpgradeFlags, env LifecycleEnv) error {
	if flags.FromSource {
		if flags.Channel != "" || flags.Release != "" || flags.Version != "" || flags.Extensions || flags.Deps {
			return cmderr.Usagef("--from-source builds the CLI from this workspace and cannot combine with --release/--channel/--version or --extensions/--deps")
		}
		iox.Fprintln(os.Stdout, "\n  CLI (from source)")
		return versioncmd.VersionInstallFromSource(ctx, wsRoot, binDir, flags.DryRun)
	}

	selector, err := resolveUpgradeSelector(flags)
	if err != nil {
		return err
	}
	if selector.Mode == "release" && (flags.CLI || flags.Extensions) {
		return cmderr.Usagef("--release selects an immutable npm/Go dependency snapshot and cannot combine with --cli or --extensions")
	}
	all := !flags.CLI && !flags.Extensions && !flags.Deps
	if selector.Mode == "release" && all {
		// release-set/v1 contains npm and Go members only. An unqualified exact
		// release therefore means the dependency phase, never mutable CLI or
		// extension registries that are outside that snapshot.
		all = false
		flags.Deps = true
	}
	selector, err = resolveUpgradeReleaseSet(ctx, wsRoot, cfg, selector)
	if err != nil {
		return err
	}

	var anyError bool
	var cliUpdate versioncmd.VersionUpdateResult

	if flags.DryRun {
		iox.Fprintln(os.Stdout, "\n  Putnami upgrade plan")
		iox.Fprintf(os.Stdout, "  Selector: %s (%s)\n", selector.Display, selector.Mode)
	}
	if selector.ReleaseSet != nil {
		iox.Fprintf(os.Stdout, "  Release set: %s (%s)\n", selector.ReleaseSet.Ref.ID, selector.ReleaseSet.Ref.Digest)
	}

	if all || flags.CLI {
		result, failed, err := upgradeCLIPhase(ctx, currentVersion, binDir, cfg, selector, flags.DryRun)
		if err != nil {
			return err
		}
		cliUpdate = result
		anyError = anyError || failed
	}
	warnWorkspaceAfterCLIUpdate(wsRoot, cliUpdate, all, flags)

	if restarting, err := restartRemainingUpgradePhases(cliUpdate, all, flags, selector); restarting {
		return err
	}

	// The content of an npm extension follows the package the package manager
	// installs, so when this upgrade runs the dependency phase, the agent phase
	// runs after it instead of inside the artifact phases.
	agentAfterDeps := (all || flags.Deps) && agentctx.AgentContentFollowsPackageManager(wsRoot, cfg)

	if all || flags.Extensions {
		anyError = upgradeArtifactPhases(ctx, wsRoot, cfg, selector, flags, !agentAfterDeps) || anyError
	}

	depsFailed := false
	if all || flags.Deps {
		depsFailed = upgradeDependencyPhase(ctx, wsRoot, cfg, selector, flags.DryRun, flags.ContinueOnError, env)
		anyError = depsFailed || anyError
	}

	// A failed dependency phase skips the deferred agent phase, as install
	// does: the previous content and record stay until a run whose package
	// manager succeeds.
	if agentAfterDeps && depsFailed {
		iox.Fprintln(os.Stdout, "\n  Agent workflows")
		iox.Fprintln(os.Stderr, "  ✗ Agent workflows update: skipped because the dependency phase failed; the previous content stays until the package manager succeeds")
	} else if agentAfterDeps {
		anyError = upgradeAgentWorkflowPhase(ctx, wsRoot, cfg, flags.DryRun) || anyError
	}

	iox.Fprintln(os.Stdout)
	if anyError {
		return fmt.Errorf("some upgrade phases failed")
	}
	return nil
}

func upgradeDependencyPhase(ctx context.Context, wsRoot string, cfg *wsproto.Config, selector upgradeSelector, dryRun, continueOnError bool, env LifecycleEnv) bool {
	iox.Fprintln(os.Stdout, "\n  Dependencies")
	if dryRun {
		iox.Fprintln(os.Stdout, "  Would run dependency hooks")
	} else if err := hooks.RunCommandHooks(ctx, cfg.Hooks, "before", []string{"deps-upgrade"}, wsRoot, false); err != nil {
		iox.Fprintf(os.Stderr, "  ✗ Dependencies hook (before): %v\n", err)
		return true
	}

	ran, err := upgradeDeps(ctx, wsRoot, cfg, selector, dryRun, continueOnError, env)
	if err != nil {
		iox.Fprintf(os.Stderr, "  ✗ Dependencies upgrade: %v\n", err)
		return true
	}
	if dryRun {
		if ran {
			iox.Fprintln(os.Stdout, "  Would run workspace installers")
		}
		return false
	}
	if !ran {
		return false
	}
	iox.Fprintln(os.Stdout, "\n  Workspace installers")
	if err := DepsInstall(ctx, wsRoot, cfg, "", "", env); err != nil {
		iox.Fprintf(os.Stderr, "  ✗ Workspace installers: %v\n", err)
		return true
	}
	return false
}

// upgradeArtifactPhases runs the registry-artifact half of an upgrade —
// extensions, templates and, when agentPhase is set, agent workflows — and
// reports whether any of them failed. Like every other upgrade phase these
// COLLECT failures instead of returning at the first one, so a dead template
// registry does not hide the state of the other two.
func upgradeArtifactPhases(ctx context.Context, wsRoot string, cfg *wsproto.Config, selector upgradeSelector, flags UpgradeFlags, agentPhase bool) bool {
	artifactOptions := extensions.ArtifactUpdateOptions{
		ConstraintOverride: selector.RegistryTarget,
		DryRun:             flags.DryRun,
	}
	anyError := false

	iox.Fprintln(os.Stdout, "\n  Extensions")
	if err := extensions.ExtensionsUpdateWithOptions(ctx, wsRoot, cfg, nil, artifactOptions, ""); err != nil {
		iox.Fprintf(os.Stderr, "  ✗ Extensions update: %v\n", err)
		anyError = true
	} else if !flags.DryRun {
		updatedCfg := wsproto.Load(wsRoot)
		if err := agentctx.ContextGenerate(wsRoot, updatedCfg, nil); err != nil {
			iox.Fprintf(os.Stderr, "  AI context generation failed: %v\n", err)
		}
	} else {
		iox.Fprintln(os.Stdout, "  Would regenerate AI context")
	}
	// The putnami MCP server is registered whatever the extension update
	// did: an agent session reaches the local tools either way (ADR 0040).
	if flags.DryRun {
		iox.Fprintln(os.Stdout, "  Would register the putnami MCP server in .mcp.json unless an entry exists")
	} else if agentctx.RegisterMCPServer(wsRoot, os.Stderr) {
		iox.Fprintln(os.Stdout, "  ✓ putnami MCP server registered in .mcp.json")
	}

	iox.Fprintln(os.Stdout, "\n  Templates")
	if err := extensions.TemplatesUpdateWithOptions(ctx, wsRoot, cfg, nil, artifactOptions, ""); err != nil {
		iox.Fprintf(os.Stderr, "  ✗ Templates update: %v\n", err)
		anyError = true
	}

	if agentPhase {
		anyError = upgradeAgentWorkflowPhase(ctx, wsRoot, cfg, flags.DryRun) || anyError
	}
	return anyError
}

// upgradeAgentWorkflowPhase is the agent phase of an upgrade, and reports
// whether it failed.
//
// Agent content follows the extensions the workspace already opts into:
// upgrade never adds content a workspace did not ask for. The phase is
// transactional (ADR 0047 §4) — a failure here leaves the previous files and
// ownership records — so joining the collect-and-report loop costs nothing in
// partial state. A workspace that stopped opting into content still enters the
// phase: removing the opt-in is how a user removes the content, and upgrade is
// where its files and record are retired.
func upgradeAgentWorkflowPhase(ctx context.Context, wsRoot string, cfg *wsproto.Config, dryRun bool) bool {
	workflowPhase, phaseErr := agentctx.AgentWorkflowPhaseApplies(wsRoot, cfg)
	if phaseErr != nil {
		iox.Fprintln(os.Stdout, "\n  Agent workflows")
		iox.Fprintf(os.Stderr, "  ✗ Agent workflows update: %v\n", phaseErr)
		return true
	}
	if !workflowPhase {
		return false
	}
	iox.Fprintln(os.Stdout, "\n  Agent workflows")
	if dryRun {
		agentctx.DescribeAgentWorkflowPlan(wsRoot, cfg, os.Stdout)
		return false
	}
	if err := agentctx.AdoptAgentWorkflows(ctx, wsRoot, cfg, os.Stdout); err != nil {
		iox.Fprintf(os.Stderr, "  ✗ Agent workflows update: %v\n", err)
		return true
	}
	return false
}

func upgradeCLIPhase(ctx context.Context, currentVersion, binDir string, cfg *wsproto.Config, selector upgradeSelector, dryRun bool) (versioncmd.VersionUpdateResult, bool, error) {
	iox.Fprintln(os.Stdout, "\n  CLI")
	var registries map[string]json.RawMessage
	if cfg != nil {
		registries = cfg.Registries
	}
	result, err := versionUpdateWithOptions(ctx, currentVersion, binDir, versioncmd.VersionUpdateOptions{
		Channel:    selector.RegistryTarget,
		DryRun:     dryRun,
		Registries: registries,
	})
	if err != nil {
		if errors.Is(err, versioncmd.ErrCLIActivation) {
			return versioncmd.VersionUpdateResult{}, true, cliActivationFailure(err)
		}
		iox.Fprintf(os.Stderr, "  ✗ CLI update: %v\n", err)
		return versioncmd.VersionUpdateResult{}, true, nil
	}
	if dryRun {
		iox.Fprintln(os.Stdout, "  Would refresh shell completions")
		return versioncmd.VersionUpdateResult{}, false, nil
	}
	if err := refreshCompletionsAfterCLIUpdateFn(ctx, binDir); err != nil {
		iox.Fprintf(os.Stderr, "  ! Shell completions: %v\n", err)
	}
	return result, false, nil
}

// restartRemainingUpgradePhases hands remaining workspace phases to a CLI that
// was installed during this invocation. A process cannot update itself, and an
// old process may not support the runtime contract of the extensions it just
// downloaded (for example, {extensionRuntime}). The restart deliberately
// bypasses the workspace pin, which can still name that old process.
func warnWorkspaceAfterCLIUpdate(wsRoot string, cliUpdate versioncmd.VersionUpdateResult, all bool, flags UpgradeFlags) {
	if !cliUpdate.Updated {
		return
	}
	if warning := workspaceCLIPinUpgradeWarning(wsRoot, cliUpdate.Version, all || flags.Extensions || flags.Deps); warning != "" {
		iox.Fprintf(os.Stderr, "  ! %s\n", warning)
	}
}

func restartRemainingUpgradePhases(cliUpdate versioncmd.VersionUpdateResult, all bool, flags UpgradeFlags, selector upgradeSelector) (bool, error) {
	if !cliUpdate.Updated || (!all && !flags.Extensions && !flags.Deps) {
		return false, nil
	}
	iox.Fprintln(os.Stdout, "  Restarting with the updated CLI for remaining phases...")
	args := upgradeRemainingArgs(all, flags, selector)
	env := upgradeRestartEnv(flags)
	if err := restartUpgradeProcess(cliUpdate.BinaryPath, args, env...); err != nil {
		return true, protocolcli.WithNext(
			fmt.Errorf("could not restart with the updated CLI %s: %w", cliUpdate.BinaryPath, err),
			directUpgradeCommand(cliUpdate.BinaryPath, env, args),
		)
	}
	return true, nil
}

func cliActivationFailure(err error) error {
	var activation *versioncmd.CLIActivationError
	if errors.As(err, &activation) && activation.BinaryPath != "" {
		return protocolcli.WithNext(
			fmt.Errorf("CLI update did not activate: %w", err),
			launch.NoRelaunchEnv+"=1 "+launch.ShellQuote(activation.BinaryPath)+" --version",
		)
	}
	return fmt.Errorf("CLI update did not activate: %w", err)
}

func workspaceCLIPinUpgradeWarning(wsRoot, version string, hasRemainingPhases bool) string {
	if wsRoot == "" || version == "" {
		return ""
	}
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil || lf == nil {
		return ""
	}
	pinned, ok := lf.GetCLI()
	if !ok {
		return ""
	}
	if pinned.IsWorkspaceSource() {
		// A source workspace records no version, so the pin-drift wording below
		// would print "remains pinned to " with an empty version. Say the true
		// thing instead: upgrading the installed CLI does not change which
		// engine this workspace runs, because that engine is built from the tree.
		return fmt.Sprintf(
			"this workspace builds its own CLI from source (%s: cli.source = %q), so the updated %s is not what its commands use; run them through ./putnamiw",
			lockfile.LockFilename, lockfile.SourceWorkspace, version)
	}
	pinnedVersion := strings.TrimPrefix(strings.TrimSpace(pinned.Version), "v")
	resolvedVersion := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if pinnedVersion == resolvedVersion {
		return ""
	}
	if hasRemainingPhases {
		return fmt.Sprintf("workspace remains pinned to %s; this upgrade's remaining phases use %s, but later commands return to the pin. To adopt it deliberately, run: putnami pin %s", pinned.Version, resolvedVersion, resolvedVersion)
	}
	return fmt.Sprintf("workspace remains pinned to %s; despite the active CLI link pointing to %s, ordinary commands continue to use the pin. To adopt it deliberately, run: putnami pin %s", pinned.Version, resolvedVersion, resolvedVersion)
}

// upgradeRestartEnv is the environment the restart sets: PUTNAMI_PROVIDERS
// when this process enabled providers, so the remaining phases download
// through the same credential provider.
func upgradeRestartEnv(flags UpgradeFlags) []string {
	if len(flags.Providers) == 0 {
		return nil
	}
	return []string{jobs.ProvidersEnv + "=" + strings.Join(flags.Providers, ",")}
}

func directUpgradeCommand(binaryPath string, env, args []string) string {
	parts := []string{launch.NoRelaunchEnv + "=1"}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		parts = append(parts, name+"="+launch.ShellQuote(value))
	}
	parts = append(parts, launch.ShellQuote(binaryPath))
	for _, arg := range args {
		parts = append(parts, launch.ShellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func upgradeRemainingArgs(all bool, flags UpgradeFlags, selector upgradeSelector) []string {
	args := []string{"upgrade"}
	if all || flags.Extensions {
		args = append(args, "--extensions")
	}
	if all || flags.Deps {
		args = append(args, "--deps")
	}
	switch selector.Mode {
	case "version":
		args = append(args, "--version", selector.Display)
	case "release":
		args = append(args, "--release", selector.Display)
		if selector.Namespace != "" {
			args = append(args, "--namespace", selector.Namespace)
		}
	default:
		if selector.ExplicitChannel {
			args = append(args, "--channel", selector.Display)
		}
	}
	return args
}

func refreshCompletionsAfterCLIUpdate(ctx context.Context, binDir string) error {
	result, err := completion.RefreshShellCompletions(ctx, versioncmd.CLIPath(binDir))
	if err != nil {
		return err
	}
	if result.SkippedReason != "" {
		iox.Fprintf(os.Stdout, "  Shell completions skipped: %s\n", result.SkippedReason)
		return nil
	}
	if result.Path == "" {
		return nil
	}
	iox.Fprintf(os.Stdout, "  Shell completions refreshed: %s\n", result.Path)
	if result.ZshCacheHint {
		iox.Fprintln(os.Stdout, "  Restart zsh or run: rm -f ~/.zcompdump* && exec zsh")
	}
	if len(result.ZshDuplicates) > 1 {
		iox.Fprintln(os.Stdout, "  Multiple zsh completion files were found; remove stale duplicates if completion still looks old:")
		for _, path := range result.ZshDuplicates {
			iox.Fprintf(os.Stdout, "    %s\n", path)
		}
	}
	return nil
}

// upgradeDeps runs the "deps-upgrade" job from all extensions that provide it.
func upgradeDeps(ctx context.Context, wsRoot string, cfg *wsproto.Config, selector upgradeSelector, dryRun, continueOnError bool, env LifecycleEnv) (bool, error) {
	commandParams := map[string]any{
		"putnami-selector": selector.Display,
		"putnami-channel":  selector.RegistryTarget,
		"putnami-version":  selector.DepsTarget,
		"dry-run":          dryRun,
	}
	if len(cfg.Registries) > 0 {
		// Each ecosystem resolves its own registry from this entry rather than
		// from a hard-coded URL or an .npmrc scan.
		commandParams["registries"] = cfg.Registries
	}
	if selector.ReleaseSet != nil {
		// Carry the validated typed response verbatim. TS and Go consumers project
		// their own members from this same immutable map; neither resolves a
		// channel nor flattens mixed member versions.
		commandParams["releaseSet"] = *selector.ReleaseSet
	}

	result, err := runWorkspaceJob(ctx, env, WorkspaceJobRequest{
		WorkspaceRoot:   wsRoot,
		Config:          cfg,
		Job:             "deps-upgrade",
		Params:          commandParams,
		ContinueOnError: continueOnError,
	})
	if err != nil {
		return false, err
	}
	switch result.Outcome {
	case WorkspaceJobMissing:
		iox.Fprintln(env.out(), "  No extension provides the \"deps-upgrade\" job. Skipping.")
		return false, nil
	case WorkspaceJobNoMatches:
		iox.Fprintln(env.out(), "  No deps-upgrade jobs matched.")
		return false, nil
	case WorkspaceJobFailed:
		return true, fmt.Errorf("deps upgrade failed")
	}
	return true, nil
}
