package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ProjectsSync scans disk for projects and updates config files to match.
// When scopes are configured, new projects discovered under a scope directory are
// added to the scope's putnami.json (with relative paths), not the root config.
// The dryRun parameter is forwarded from the global --dry-run flag.
func ProjectsSync(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, dryRun bool, env LifecycleEnv) error {
	var prune, skipInstall bool
	for _, a := range args {
		switch a {
		case "--prune":
			prune = true
		case "--skip-install":
			skipInstall = true
		}
	}

	// Resolve the workspace adapters once: they widen the scan below, they own
	// the native-manifest writes further down, and they are the providers the
	// batched probe asks. Discovery failure is not fatal — `projects sync` is a
	// recovery command, and a workspace whose extensions cannot be resolved is
	// exactly the workspace that needs it to keep working.
	adapters := discoverWorkspaceAdapters(ctx, wsRoot, cfg)
	if adapters.discoveryErr != nil {
		iox.Fprintf(os.Stdout,
			"  warning: workspace adapters could not be resolved (%v); the scan runs with core's own marker only\n",
			adapters.discoveryErr)
	}

	// Scan disk for all project paths, widened by extension-declared markers.
	discovered, err := workspace.ScanProjectPathsWithProviders(wsRoot, adapters.scopes())
	if err != nil {
		return fmt.Errorf("scan projects: %w", err)
	}

	// Build set of all currently known projects (root + scopes)
	existing := allKnownProjects(wsRoot, cfg)
	discoveredSet := make(map[string]bool, len(discovered))
	for _, p := range discovered {
		discoveredSet[p] = true
	}

	// Compute diff
	var added, removed []string
	for _, p := range discovered {
		if !existing[p] {
			added = append(added, p)
		}
	}
	if prune {
		if reason := pruneRefusalReason(wsRoot, adapters); reason != "" {
			iox.Fprintf(os.Stdout, "  warning: --prune was ignored: %s\n", reason)
			prune = false
		}
	}
	if prune {
		for p := range existing {
			if discoveredSet[p] {
				continue
			}
			// The scan's silence about a directory it was told not to enter is
			// not evidence that the project is gone. Adapter excludes are
			// unioned across providers, so a member under `build`, `testdata` or
			// `vendor` is invisible to the scan while still being perfectly
			// present on disk — and deleting its membership because of that would
			// be the scan's blind spot doing the damage.
			if workspace.ExcludedByProviderScopes(adapters.scopes(), p) {
				continue
			}
			removed = append(removed, p)
		}
		sort.Strings(removed)
	}

	// Sync project names from breadcrumb namePattern
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}

	// The divergence report needs an ADOPTED provider view to mean anything.
	// Without one, a project's source identity is core's own
	// directory-basename fallback rather than what its manifest declares, so
	// EVERY convention-named project reports a divergence that does not exist.
	// The report is not lost: the post-sync refresh below re-probes and prints
	// the set the provider answer actually shows.
	var nameChanges []string
	if ws.HasProbeView() {
		nameChanges = reportNameDivergences(ws)
	}
	replaceChanges, err := repairBootstrapClosure(wsRoot, adapters, dryRun)
	if err != nil {
		return err
	}

	if len(added) == 0 && len(removed) == 0 && len(nameChanges) == 0 && len(replaceChanges) == 0 {
		iox.Fprintln(os.Stdout, "  Putnami membership is in sync.")
	} else {
		printSyncDiff(added, removed, nameChanges, replaceChanges)
	}

	if dryRun {
		iox.Fprintln(os.Stdout, "  (dry-run: no changes written)")
		refreshWorkspaceSnapshot(ctx, wsRoot, adapters, discovered, dryRun, nameChanges)
		return nil
	}

	// Partition membership changes by scope. Everything from here to
	// runWorkspaceSyncTasks is core's half of the sync: Putnami membership and
	// scopes, `--prune`, and the canonical report. No native manifest is touched
	// by any of it.
	scopePaths := workspace.ScopePaths(wsRoot, cfg)
	rootProjects := workspace.RootProjectPaths(wsRoot, cfg)
	scopeAdded, rootAdded := partitionByScope(scopePaths, added)
	scopeRemoved, rootRemoved := partitionByScope(scopePaths, removed)

	// Update scope configs
	for _, scopePath := range scopePaths {
		scopeAdd := scopeAdded[scopePath]
		scopeRem := scopeRemoved[scopePath]
		if len(scopeAdd) == 0 && len(scopeRem) == 0 {
			continue
		}
		if err := updateScopeProjects(wsRoot, scopePath, scopeAdd, scopeRem); err != nil {
			return fmt.Errorf("update scope %s: %w", scopePath, err)
		}
		scopeCfgName := filepath.Base(wsproto.ResolveFile(filepath.Join(wsRoot, scopePath), wsproto.ConfigFilename))
		iox.Fprintf(os.Stdout, "  Updated %s/%s\n", scopePath, scopeCfgName)
	}

	// Update root config (only non-scope projects)
	removedSet := make(map[string]bool, len(rootRemoved))
	for _, p := range rootRemoved {
		removedSet[p] = true
	}
	var newRootProjects []string
	for _, p := range rootProjects {
		if !removedSet[p] {
			newRootProjects = append(newRootProjects, p)
		}
	}
	newRootProjects = append(newRootProjects, rootAdded...)
	sort.Strings(newRootProjects)

	if err := updateConfigMembership(wsRoot, scopePaths, newRootProjects); err != nil {
		return fmt.Errorf("update config: %w", err)
	}

	wsCfgName := filepath.Base(wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename))
	iox.Fprintf(os.Stdout, "\n  Updated %s\n", wsCfgName)

	// Hand every native-manifest mutation to its owning extension, before the
	// snapshot is taken: the snapshot records what the tree resolves to, and a
	// snapshot taken before the sync tasks ran would record the pre-sync names.
	//
	// They run UNCONDITIONALLY, even when core found nothing to change. Core no
	// longer knows what is out of sync inside a language manifest — that is the
	// whole point of slice C4b — so deciding "nothing to do" on a provider's
	// behalf would be guessing. It is also what keeps `projects sync` the repair
	// command for the failure it exists for: a go.mod that lost a workspace
	// replace changes no Putnami membership and no project name, so a
	// membership-gated sync would report "in sync" and repair nothing.
	if err := runWorkspaceSyncTasks(ctx, wsRoot, cfg, adapters, env); err != nil {
		return err
	}

	// The membership edits above changed what the workspace resolves to, so the
	// snapshot is refreshed from a re-read tree before the installers run.
	refreshWorkspaceSnapshot(ctx, wsRoot, adapters, discovered, dryRun, nameChanges)

	// Run workspace installers
	if !skipInstall && (len(added) > 0 || len(removed) > 0) {
		iox.Fprintln(os.Stdout, "  Running workspace installers...")
		if err := DepsInstall(ctx, wsRoot, cfg, "", "", env); err != nil {
			return fmt.Errorf("workspace install: %w", err)
		}
	}

	return nil
}

// reportNameDivergences renders the projects whose source identity disagrees
// with their resolved name.
//
// Core no longer WRITES any of those manifests: each provider's own
// `workspace-sync` task does, with its own formatting rules (and the
// three-way package.json/go.mod/pyproject.toml writer deleted in an earlier cleanup). What core
// keeps is the canonical REPORT, because it is the only side that knows what the
// resolved name is — explicit putnami.json identity, scope namePattern and
// directory fallback all applied.
func reportNameDivergences(ws *workspace.Workspace) []string {
	var changes []string
	for _, p := range ws.Projects {
		if p.SourceName == "" || p.SourceName == p.Name {
			continue
		}
		changes = append(changes, fmt.Sprintf("  ~ %s: %s → %s", p.Path, p.SourceName, p.Name))
	}
	sort.Strings(changes)
	return changes
}

// repairBootstrapClosure completes the go.mod workspace-replace closure of the
// LOCAL EXTENSION MODULES this sync must prepare a runtime from, and of nothing
// else.
//
// This is the epic's one legal residue and the narrowest form of it. The full
// closure belongs to the Go extension's own `workspace-sync` task (C4a) and is
// maintained there on every sync; core keeps only the part that has to happen
// BEFORE an extension runtime can be built, because a local extension whose own
// go.mod is missing a workspace replace cannot be compiled in module mode and
// therefore cannot run the task that would fix it. See
// workspace.RepairBootstrapGoModClosure for why the ordering is real and what
// would remove it.
//
// An extension that already has a prepared runtime needs no repair, so it is not
// named: this is a bootstrap, not a second closure maintainer.
func repairBootstrapClosure(
	wsRoot string, adapters workspaceAdapters, dryRun bool,
) ([]workspace.GoModReplaceChange, error) {
	dirs := make([]string, 0, len(adapters.adapters))
	for _, adapter := range adapters.adapters {
		ext := adapter.ext
		if ext == nil || ext.RuntimeExecutable != "" || ext.RelPath == "" {
			continue
		}
		dirs = append(dirs, filepath.ToSlash(strings.TrimLeft(ext.RelPath, "/")))
	}
	changes, err := workspace.RepairBootstrapGoModClosure(wsRoot, dirs, dryRun)
	if err != nil {
		return nil, fmt.Errorf("repair extension bootstrap go.mod closure: %w", err)
	}
	return changes, nil
}

// workspaceAdapter is ONE resolved extension together with the provider binding
// it declared.
//
// The pairing is a struct rather than two parallel slices because the two used
// to be exactly that, appended in lockstep in DISCOVERY order and then sorted on
// one side only — so `a.bindings[i]` and `a.extensions[i]` described different
// extensions the moment discovery order was not extension-name order. What that
// bought was a `syncCommands()` that looked up one extension's declared task in
// ANOTHER extension's job table, found nothing, and returned an empty command
// list: `projects sync` then exited 0 having run no provider's sync task at all.
// It was masked only because the three shipped extensions happen to spell
// `syncTask` identically, which syncCommands' own doc comment says core must
// never assume, and discovery source #3 iterates a Go map, so the mispairing was
// nondeterministic per process. One slice of pairs makes the bug unrepresentable.
type workspaceAdapter struct {
	ext     *extension.ExtensionDescription
	binding workspace.ProviderBinding
}

// workspaceAdapters is the resolved set of extensions that participate in
// project discovery, in extension-name order.
type workspaceAdapters struct {
	adapters []workspaceAdapter
	// skipped names the extensions discovery FOUND but could not load: a
	// manifest that failed to parse, an artifact that is not materialized, a
	// contract newer than this CLI. A skip is the fact "this run could not
	// resolve it", which is precisely what discoveryErr was meant to carry and
	// never could — DiscoverExtensionsDetailed has one return and it never
	// errors, so an unresolvable extension reference lands in
	// DiscoveryResult.Skipped instead.
	skipped []string
	// discoveryErr records why this set may be INCOMPLETE. An empty set is two
	// different facts — "this workspace has no adapters" and "this run could not
	// resolve them" — and `--prune` acts destructively on the difference, so the
	// cause is carried rather than swallowed.
	discoveryErr error
}

// bindings is the provider set the probe and the snapshot refresh are driven
// from, in the same extension-name order the adapters are held in.
func (a workspaceAdapters) bindings() []workspace.ProviderBinding {
	bindings := make([]workspace.ProviderBinding, 0, len(a.adapters))
	for _, adapter := range a.adapters {
		bindings = append(bindings, adapter.binding)
	}
	return bindings
}

// pruneRefusalReason answers whether `--prune` may act on this scan, and names
// the reason when it may not.
//
// `--prune` deletes Putnami membership for every declared project the scan did
// NOT return. The scan only returns what core's own marker (putnami.json) and
// the resolved adapters' markers claim — so with no adapters resolved, a
// workspace whose members are package.json/go.mod/pyproject.toml directories
// reports EVERY one of them as removed, and one failed extension resolution
// silently empties the workspace config. The recorded index is the evidence
// that adapters are expected: it names the providers that answered last time.
//
// This is a refusal to prune, not a refusal to sync. `projects sync` is the
// command a broken workspace is repaired with, so the additive half — the scan,
// the adapters' own sync tasks, the index rebuild — must still run.
//
// FOUR arms now, ordered most-specific-first, because the recorded-index arm
// alone cannot carry the FRESH-CLONE case: `.putnami/` is gitignored, so on
// every fresh clone or new worktree the index is absent,
// recordedProviderExtensions returns nothing, and the recorded-vs-resolved
// comparison is vacuously satisfied. A clone whose extensions are not installed
// yet would then prune every package.json / go.mod / pyproject.toml member on
// the very first `projects sync --prune` — reproduced with the real CLI, which
// printed `0 added, N removed` and emptied `includes` while every file was still
// on disk. The two added arms are the direct evidence for that state: discovery
// skipped an extension it found, or no adapter resolved at all.
func pruneRefusalReason(wsRoot string, adapters workspaceAdapters) string {
	if adapters.discoveryErr != nil {
		return fmt.Sprintf("the workspace adapters could not be resolved (%v), so the scan cannot see "+
			"any project that has no putnami.json; re-run once extensions resolve", adapters.discoveryErr)
	}

	// A skipped extension is one discovery FOUND and could not load. Whether it
	// declares an adapter is unknowable — its manifest is exactly what failed to
	// parse — so it is treated as if it did: the cheap wrong answer here is a
	// prune that did not happen, and the expensive one is membership deleted
	// because a marker went unrecognized for one run.
	if len(adapters.skipped) > 0 {
		skipped := append([]string(nil), adapters.skipped...)
		sort.Strings(skipped)
		return fmt.Sprintf("%d extension(s) were skipped during discovery (%s), so an adapter they declare "+
			"could not widen the scan and every project only its markers can find would look deleted; "+
			"re-run once those extensions load", len(skipped), strings.Join(skipped, ", "))
	}

	// The recorded index names the providers that answered last time, so when it
	// disagrees with what resolved now it is the most precise thing this function
	// can say. It is checked before the zero-adapter arm for that reason: both
	// describe "an adapter is missing", and this one can name it.
	recorded := recordedProviderExtensions(wsRoot)
	resolved := make(map[string]bool, len(adapters.adapters))
	for _, adapter := range adapters.adapters {
		resolved[adapter.binding.Scope.Extension] = true
	}
	var missing []string
	for _, extensionName := range recorded {
		if !resolved[extensionName] {
			missing = append(missing, extensionName)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Sprintf("%d of the %d provider(s) recorded in %s did not resolve for this run (%s), "+
			"so every project only they can discover would look deleted",
			len(missing), len(recorded), workspace.WorkspaceIndexFilename, strings.Join(missing, ", "))
	}

	// Zero adapters resolved and nothing recorded to compare against. The scan
	// then recognizes core's own putnami.json and nothing else, so EVERY member
	// that is a package.json / go.mod / pyproject.toml directory is reported as
	// removed. On a fresh clone with uninstalled extensions that is the whole
	// workspace — and there is no index to notice it with, because the index is
	// the file the clone does not have.
	if len(adapters.adapters) == 0 {
		return fmt.Sprintf("no workspace adapter resolved for this run, so the scan recognizes only %s and "+
			"every member discovered through an extension-declared marker would look deleted; "+
			"run `putnami install` first, then re-run with --prune", wsproto.ConfigFilename)
	}
	return ""
}

// recordedProviderExtensions names the providers the persisted index records an
// answer for. An unreadable or absent index yields nothing, which is the
// permissive answer: a workspace that has never been probed has no expectation
// to violate.
func recordedProviderExtensions(wsRoot string) []string {
	snapshot, err := workspace.LoadSnapshot(wsRoot)
	if err != nil || snapshot == nil {
		return nil
	}
	names := make([]string, 0, len(snapshot.Providers))
	for _, provider := range snapshot.Providers {
		names = append(names, provider.Extension)
	}
	return names
}

// discoverWorkspaceAdapters resolves every extension that declares a workspace
// adapter, binding each to a lazily-prepared runtime probe provider.
//
// Lazy is the operative word: the provider's runtime is prepared inside its
// first Probe call, so an adapter that is never asked never compiles. Discovery
// failure yields an EMPTY set rather than an error — `projects sync` is one of
// the recovery commands, and refusing to run it because an extension could not
// be resolved would make a broken workspace unrepairable.
//
// The failure is RECORDED though, on discoveryErr. An empty set for the wrong
// reason is what made `--prune` delete marker-discovered membership silently:
// the caller has to be able to tell "this workspace has no adapters"
// from "this run could not resolve them".
func discoverWorkspaceAdapters(ctx context.Context, wsRoot string, cfg *wsproto.Config) workspaceAdapters {
	var adapters workspaceAdapters

	var projectPaths []string
	if ws, err := workspace.Load(wsRoot); err == nil {
		projectPaths = make([]string, 0, len(ws.Projects))
		for _, project := range ws.Projects {
			projectPaths = append(projectPaths, project.Path)
		}
	}
	discovered, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths)
	if err != nil {
		adapters.discoveryErr = err
		return adapters
	}
	if discovered == nil {
		adapters.discoveryErr = errors.New("extension discovery returned no result")
		return adapters
	}
	for _, skip := range discovered.Skipped {
		name := skip.Name
		if name == "" {
			name = skip.Ref
		}
		if name == "" {
			name = skip.Path
		}
		adapters.skipped = append(adapters.skipped, name)
	}
	for _, ext := range discovered.Extensions {
		if ext == nil || ext.Workspace == nil {
			continue
		}
		scope, ok := workspace.NewProviderScope(ext.Name, ext.Workspace)
		if !ok {
			continue
		}
		adapters.adapters = append(adapters.adapters, workspaceAdapter{
			ext: ext,
			binding: workspace.ProviderBinding{
				Scope: scope,
				Provider: &workspace.ExecProbeProvider{
					Extension:         ext.Name,
					Dir:               wsRoot,
					Resolve:           syncRuntimeResolver(ctx, wsRoot, ext),
					FromArtifactStore: extension.InArtifactStore(wsRoot, ext),
				},
			},
		})
	}
	// Sorted ONCE, over the pairs. Sorting a binding slice while its extension
	// slice kept discovery order is what silently unpaired the two.
	sort.SliceStable(adapters.adapters, func(i, j int) bool {
		return adapters.adapters[i].binding.Scope.Extension < adapters.adapters[j].binding.Scope.Extension
	})
	return adapters
}

// syncRuntimeResolver prepares one extension's runtime on first use.
//
// The command's context travels into the preparation: a local extension's
// runtime is COMPILED here, and a canceled `projects sync` that left a compile
// running would keep the toolchain busy after the user has already been given
// their prompt back.
func syncRuntimeResolver(ctx context.Context, wsRoot string, ext *extension.ExtensionDescription) func() (string, error) {
	return func() (string, error) {
		if ext.RuntimeExecutable != "" {
			return ext.RuntimeExecutable, nil
		}
		ws, err := workspace.Load(wsRoot)
		if err != nil {
			return "", err
		}
		if err := jobs.SynchronizeExtensionRuntimes(ctx, ws,
			[]*extension.ExtensionDescription{ext}, nil); err != nil {
			return "", err
		}
		return ext.RuntimeExecutable, nil
	}
}

func (a workspaceAdapters) scopes() []workspace.ProviderScope {
	scopes := make([]workspace.ProviderScope, 0, len(a.adapters))
	for _, adapter := range a.adapters {
		scopes = append(scopes, adapter.binding.Scope)
	}
	return scopes
}

// syncCommands names the extension COMMANDS that run the declared sync tasks.
//
// The adapter names a TASK; the workspace-lifecycle runner selects a COMMAND.
// The two are matched by finding the single-step command whose step runs that
// task, so an extension is free to name them differently and core never assumes
// they are the same string.
//
// The task and the job table therefore have to come from the SAME extension —
// which is why the adapters are held as pairs. Reading them out of two slices
// sorted differently looked up one extension's task in another's jobs and
// returned nothing at all (see workspaceAdapter).
func (a workspaceAdapters) syncCommands() []string {
	var commands []string
	seen := make(map[string]bool)
	for _, adapter := range a.adapters {
		task := adapter.binding.Scope.SyncTask
		if task == "" || adapter.ext == nil {
			continue
		}
		for name, job := range adapter.ext.Jobs {
			if len(job.PipelineSteps) != 1 || job.PipelineSteps[0].Task != task || seen[name] {
				continue
			}
			seen[name] = true
			commands = append(commands, name)
		}
	}
	sort.Strings(commands)
	return commands
}

// runWorkspaceSyncTasks hands the native-manifest mutations to their owning
// extensions.
//
// Each declared sync command runs as a workspace-level lifecycle job, the same
// way `workspace-install` does: one job per providing extension, uncached, over
// the whole project selection. A missing or unmatched command is a no-op rather
// than a failure, because an extension may declare an adapter without a
// syncTask; a FAILING sync task is fatal, because it means a manifest core no
// longer writes did not get written by anyone.
func runWorkspaceSyncTasks(
	ctx context.Context,
	wsRoot string,
	cfg *wsproto.Config,
	adapters workspaceAdapters,
	env LifecycleEnv,
) error {
	commands := adapters.syncCommands()
	if len(commands) == 0 {
		return nil
	}
	// The membership edits above are on disk but the memoized workspace still
	// describes the pre-sync tree; the sync tasks receive the resolved project
	// selection, so it must be the post-sync one.
	workspace.InvalidateLoadCache(wsRoot)

	for _, command := range commands {
		result, err := runWorkspaceJob(ctx, env, WorkspaceJobRequest{
			WorkspaceRoot: wsRoot,
			Config:        cfg,
			Job:           command,
			Out:           env.out(),
		})
		if err != nil {
			return fmt.Errorf("workspace sync (%s): %w", command, err)
		}
		switch result.Outcome {
		case WorkspaceJobFailed:
			return fmt.Errorf("workspace sync (%s) failed", command)
		case WorkspaceJobMissing, WorkspaceJobNoMatches:
			iox.Fprintf(os.Stdout, "  note: no extension ran %s\n", command)
		}
	}
	return nil
}

// refreshWorkspaceSnapshot rebuilds `.putnami/workspace-index.json` for the
// post-sync tree.
//
// `projects sync` is the FULL scan, so it is the command that rebuilds the
// digest-authoritative index from scratch: it re-probes every provider over the
// directories the marker scan found, including directories that were not
// members before this run. Ordinary loads only re-hash the recorded metadata
// inputs and re-probe what moved.
//
// The workspace is re-loaded from disk first because the sync just rewrote
// membership, and the memoized *Workspace still describes the pre-sync tree —
// recording that would persist a snapshot of a workspace that no longer exists.
//
// Under `--dry-run` the snapshot is built in memory and the write is refused by
// the write policy, not by this caller remembering to skip it. Every failure —
// including a probe failure — warns and continues: `projects sync` is the
// command that REPAIRS a broken workspace, so it must never be the command a
// broken extension takes down.
// Every diagnostic the synchronization produced is printed. A run that kept a
// stale index because no adapter resolved, or that refused a recorded view it
// could not adopt, used to look exactly like a run that had nothing to do;
// the outcome carries the reason, so this prints it.
//
// `reported` is the divergence set the load-time report already printed. When
// the load had no provider view, that set is empty on purpose and the real one
// is printed here — this is where the provider's answer finally exists.
func refreshWorkspaceSnapshot(
	ctx context.Context,
	wsRoot string,
	adapters workspaceAdapters,
	candidates []string,
	dryRun bool,
	reported []string,
) {
	workspace.InvalidateLoadCache(wsRoot)
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		iox.Fprintf(os.Stdout, "  warning: could not reload the workspace for the snapshot: %v\n", err)
		return
	}
	bindings := adapters.bindings()
	for i := range bindings {
		if provider, ok := bindings[i].Provider.(*workspace.ExecProbeProvider); ok {
			provider.Context = ctx
		}
	}
	outcome, err := workspace.Synchronize(workspace.SyncRequest{
		Context: ctx,
		OnLockWait: func(message string) {
			iox.Fprintf(os.Stderr, "putnami: %s\n", message)
		},
		Workspace:      ws,
		Providers:      bindings,
		Reason:         wsproto.ProbeReasonRefresh,
		Policy:         workspace.SnapshotWritePolicy{DryRun: dryRun},
		Full:           true,
		CandidatePaths: probeCandidatePaths(ws, candidates),
	})
	if err != nil {
		failure := &wsproto.ProbeFailure{}
		if errors.As(err, &failure) && workspace.RefusesAUsableGraph(failure) {
			// The refusal is a verdict on a complete answer; the user fixes it
			// finding by finding.
			iox.Fprintf(os.Stdout, "  warning: %s: %s\n", failure.Kind, failure.Message)
			for _, d := range failure.Diagnostics {
				iox.Fprintf(os.Stdout, "  %s\n", d.String())
			}
			printRefreshedNameDivergences(ws, reported, false)
			return
		}
		iox.Fprintf(os.Stdout, "  warning: workspace probe failed; the index was not rebuilt: %v\n", err)
		printRefreshedNameDivergences(ws, reported, false)
		return
	}
	for _, d := range outcome.Diagnostics {
		if d.Severity == diag.Error {
			continue // an error-severity finding only travels with a failure, handled above
		}
		iox.Fprintf(os.Stdout, "  warning: %s\n", d.String())
	}
	ws.AdoptProbeView(outcome.Merged)
	printRefreshedNameDivergences(ws, reported, ws.HasProbeView())
}

// printRefreshedNameDivergences prints the divergence report the load-time one
// could not produce, and stays quiet when it already said the same thing.
//
// Two runs arrive here with different knowledge. One adopted a recorded provider
// view at load time and has already reported what that view showed; it only
// speaks up when the fresh answer disagrees. The other had no view — a workspace
// with no index, or one whose index was rejected — so its load-time report was
// suppressed on purpose, because before any manifest has been read every
// convention-named project looks unaligned. When no provider answered at all the
// lines are still printed, and labeled for what they are: core's own fallback
// identity, not a manifest's.
func printRefreshedNameDivergences(ws *workspace.Workspace, reported []string, verified bool) {
	fresh := reportNameDivergences(ws)
	if len(fresh) == 0 || slices.Equal(fresh, reported) {
		return
	}
	if verified {
		iox.Fprintln(os.Stdout, "\n  Names to align:")
	} else {
		iox.Fprintln(os.Stdout,
			"\n  Names to align (unverified: no provider answered, so the source identity is core's own fallback):")
	}
	for _, line := range fresh {
		iox.Fprintln(os.Stdout, line)
	}
}

// probeCandidatePaths is the directory set providers are asked about: every
// directory the marker scan found, plus every resolved project and the
// workspace root. The union matters — a scan-only directory is how a brand-new
// project becomes visible, and a member with no marker is how core learns a
// provider does NOT claim it.
func probeCandidatePaths(ws *workspace.Workspace, scanned []string) []string {
	paths := make([]string, 0, len(scanned)+len(ws.Projects)+1)
	paths = append(paths, wsproto.ProbeRootPath)
	for _, scan := range scanned {
		if normalized, ok := wsproto.NormalizeProbePath(scan); ok {
			paths = append(paths, normalized)
		}
	}
	for _, project := range ws.Projects {
		if normalized, ok := wsproto.NormalizeProbePath(project.Path); ok {
			paths = append(paths, normalized)
		}
	}
	sort.Strings(paths)

	deduped := paths[:0]
	previous := ""
	for i, value := range paths {
		if i > 0 && value == previous {
			continue
		}
		previous = value
		deduped = append(deduped, value)
	}
	return deduped
}

// printSyncDiff prints the sync change report: discovered/pruned projects,
// aligned names, and per-module workspace replace additions, followed by a
// one-line summary.
func printSyncDiff(added, removed, nameChanges []string, replaceChanges []workspace.GoModReplaceChange) {
	iox.Fprintln(os.Stdout)
	for _, p := range added {
		iox.Fprintf(os.Stdout, "  + %s\n", p)
	}
	for _, p := range removed {
		iox.Fprintf(os.Stdout, "  - %s\n", p)
	}
	for _, c := range nameChanges {
		iox.Fprintln(os.Stdout, c)
	}
	replacesAdded := 0
	for _, ch := range replaceChanges {
		replacesAdded += len(ch.Added)
		short := make([]string, len(ch.Added))
		for i, mod := range ch.Added {
			short[i] = shortModuleName(mod)
		}
		iox.Fprintf(os.Stdout, "  ~ %s/go.mod: +%d workspace replaces (%s)\n", ch.Dir, len(ch.Added), strings.Join(short, ", "))
	}
	// "to align", not "aligned": core stopped WRITING those manifests in slice
	// C4b — each provider's own sync task does — so the count is a report, and
	// claiming the alignment happened here would be claiming someone else's work.
	summary := fmt.Sprintf("\n  %d added, %d removed, %d names to align", len(added), len(removed), len(nameChanges))
	if replacesAdded > 0 {
		summary += fmt.Sprintf(", %d workspace replaces added", replacesAdded)
	}
	iox.Fprintln(os.Stdout, summary)
}

// shortModuleName trims the host element from a module path for compact diff
// output (go.putnami.dev/protocol/config → protocol/config).
func shortModuleName(mod string) string {
	if idx := strings.Index(mod, "/"); idx >= 0 {
		return mod[idx+1:]
	}
	return mod
}

// allKnownProjects returns a set of all project paths currently declared
// (in root config and all scope configs).
func allKnownProjects(wsRoot string, cfg *wsproto.Config) map[string]bool {
	known := make(map[string]bool)
	for _, p := range workspace.RootProjectPaths(wsRoot, cfg) {
		known[p] = true
	}
	for _, scopePath := range workspace.ScopePaths(wsRoot, cfg) {
		sc := wsproto.ReadScopeConfig(filepath.Join(wsRoot, scopePath))
		if sc == nil {
			continue
		}
		for _, relProj := range sc.IncludePaths() {
			known[filepath.Join(scopePath, relProj)] = true
		}
	}
	return known
}

// partitionByScope splits project paths into per-scope buckets and a root bucket.
// A project belongs to a scope if its path starts with the scope path prefix.
// The longest matching scope wins (most specific).
func partitionByScope(scopes []string, paths []string) (map[string][]string, []string) {
	// Sort scopes by length descending so longest match wins
	sorted := make([]string, len(scopes))
	copy(sorted, scopes)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i]) > len(sorted[j])
	})

	scopeMap := make(map[string][]string)
	var root []string

	for _, p := range paths {
		matched := false
		for _, scopePath := range sorted {
			if relPath, ok := strings.CutPrefix(p, scopePath+"/"); ok {
				scopeMap[scopePath] = append(scopeMap[scopePath], relPath)
				matched = true
				break
			}
		}
		if !matched {
			root = append(root, p)
		}
	}

	return scopeMap, root
}

// updateScopeProjects adds/removes projects from a scope's config file.
// Project paths are relative to the scope directory.
func updateScopeProjects(wsRoot, scopePath string, added, removed []string) error {
	cfgPath := wsproto.ResolveFile(filepath.Join(wsRoot, scopePath), wsproto.ConfigFilename)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var projects []string
	if rawProjects, ok := raw["includes"]; ok {
		if arr, ok := rawProjects.([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					projects = append(projects, s)
				}
			}
		}
	}

	// Remove
	removedSet := make(map[string]bool, len(removed))
	for _, p := range removed {
		removedSet[p] = true
	}
	var filtered []string
	for _, p := range projects {
		if !removedSet[p] {
			filtered = append(filtered, p)
		}
	}

	// Add
	filtered = append(filtered, added...)
	sort.Strings(filtered)

	raw["includes"] = filtered
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, append(out, '\n'), 0o644)
}

func updateConfigMembership(wsRoot string, scopes, projects []string) error {
	cfgPath := wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename)
	raw, err := jsonutil.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(scopes)+len(projects))
	includes := make([]string, 0, len(scopes)+len(projects))
	for _, path := range append(scopes, projects...) {
		if seen[path] {
			continue
		}
		seen[path] = true
		includes = append(includes, path)
	}
	sort.Strings(includes)
	raw.Set("includes", includes)
	return jsonutil.WriteFile(cfgPath, raw)
}
