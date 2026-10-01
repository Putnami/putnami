package workspace

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"

	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The DIRECT SYNC SERVICE.
//
// One function decides, for every command, what the workspace's provider view
// is: `Synchronize`. Ordinary loads, watch iterations and `projects sync` all
// enter here, which is what makes these three load-path invariants
// checkable in one place instead of asserted three times:
//
//  1. SNAPSHOT FIRST. When the persisted snapshot's recorded metadata inputs
//     still hash to their recorded digests, no provider runs — not one process.
//     An ordinary source-only edit therefore costs zero probes. Source shape a
//     provider explicitly declares as workspace metadata is different: its
//     change correctly invalidates that provider's recorded answer.
//
//  2. ONLY WHAT MOVED. When the snapshot is stale, exactly the providers whose
//     own declared inputs moved are re-probed. Every other provider's recorded
//     answer is carried forward from the snapshot, because "probe only what
//     changed" without carrying the rest forward would silently drop the
//     unprobed providers' dependency edges.
//
//  3. NEVER SPECULATIVE. `--plan` and `--dry-run` may probe in memory; the
//     write is refused by SnapshotWritePolicy, not by each caller remembering
//     to skip it. Persisting a planning run's view would let a hypothetical
//     selection become the workspace's recorded identity.
//
// A changed input that NO provider claims — a putnami.json, the workspace
// config, a scope config — invalidates every provider. Those files decide which
// directories are candidates and what their explicit identity is, so a provider
// that was not re-asked would be answering about a membership that no longer
// exists.

// ProviderBinding pairs a declared adapter with the provider that answers for
// it. The two are separate because the scope comes from a manifest and the
// provider comes from a prepared runtime; tests supply a fixture provider with
// the real scope, and that is the same code path production takes.
type ProviderBinding struct {
	Scope    ProviderScope
	Provider ProbeProvider
	// Implementation identifies the provider implementation this binding
	// runs (SnapshotProvider.Implementation). Empty when the caller cannot
	// name it; the recorded answer then stands on its inputs alone.
	Implementation string
}

// SyncRequest is one workspace synchronization.
type SyncRequest struct {
	// Context bounds both waiting for another process's index preparation and
	// the providers this synchronization starts. Nil means Background.
	Context context.Context
	// OnLockWait receives one human-readable notice when lock contention lasts
	// longer than one second. Callers route it to their safe diagnostic stream;
	// nil keeps library and test uses silent.
	OnLockWait func(string)
	// Workspace is the loaded workspace. Its resolved projects are the
	// candidate directories providers are asked about.
	Workspace *Workspace
	// Providers are the bound adapters. An empty set is a no-op that still
	// reports the snapshot state, so a workspace with no adapter-declaring
	// extension never pays for this phase.
	Providers []ProviderBinding
	// Reason travels to the providers. Advisory: a provider must answer the
	// same facts for the same tree regardless.
	Reason wsproto.ProbeReason
	// Policy decides whether this run may persist the snapshot.
	Policy SnapshotWritePolicy
	// Full forces a complete re-probe of every provider, ignoring a valid
	// snapshot. `projects sync` sets it: it is the full scan, and its whole job
	// is to rebuild the index rather than to trust it.
	Full bool
	// CandidatePaths overrides the candidate directory set. Empty means the
	// workspace's resolved projects. `projects sync` supplies the marker scan's
	// result, which is how a directory that is not yet a member becomes one.
	CandidatePaths []string
}

// SyncOutcome is what one synchronization resolved.
type SyncOutcome struct {
	// Snapshot is the snapshot that now describes the workspace — the validated
	// existing one, or the newly built one.
	Snapshot *Snapshot
	// Fresh reports that the RECORDED snapshot still describes the workspace, so
	// nothing was rewritten. That happens two ways: the snapshot validated and no
	// provider ran (the zero-process path), or a full re-probe reproduced it
	// exactly (`projects sync` over an unchanged tree). Fresh is therefore NOT
	// the answer to "did a process start" — Probed is.
	Fresh bool
	// Reason names why a probe was needed, for diagnostics. Empty when fresh.
	Reason string
	// Probed lists the extensions actually asked, in name order. Empty on a
	// fresh snapshot — the assertion the zero-process invariant is checked with.
	Probed []string
	// Changed lists the workspace-relative inputs whose content moved.
	Changed []string
	// Merged is the merged, core-owned view keyed by canonical project path.
	Merged map[string]wsproto.MergedProject
	// Diagnostics are provider findings and core's own advisories.
	Diagnostics []diag.Diagnostic
	// Persisted reports that the new snapshot reached disk.
	Persisted bool
}

// Synchronize resolves the workspace's provider view, snapshot-first.
//
// A probe failure is returned as a typed *wsproto.ProbeFailure and the outcome
// is nil: the caller decides which commands that takes down (RequireProbe).
// Everything else — an unreadable snapshot, an unwritable `.putnami` — is
// reported as a diagnostic and recovered from, because a workspace whose index
// cannot be read must still be repairable.
func Synchronize(req SyncRequest) (*SyncOutcome, error) {
	outcome, err := synchronize(req)
	if err != nil || outcome == nil {
		return outcome, err
	}
	outcome, err = checkVisibility(req, outcome)
	if err != nil || outcome == nil {
		return outcome, err
	}
	return checkDeclaredEdges(req, outcome)
}

// checkDeclaredEdges holds the synchronized graph to the imports behind it.
//
// It runs beside the visibility check, on the same graph, for the same reason:
// only here does every edge carry the manifest family it came from, so a
// declaration no import backs is finally distinguishable from an import. The
// two checks are independent — a workspace enforces phantom edges while it
// still repairs its boundaries, or the reverse — so they carry separate
// switches and separate refusals.
func checkDeclaredEdges(req SyncRequest, outcome *SyncOutcome) (*SyncOutcome, error) {
	var config *wsproto.Config
	if req.Workspace != nil {
		config = req.Workspace.Config
	}
	mode, err := ResolveDeclaredEdgesMode(config)
	if err != nil {
		outcome.Diagnostics = append(outcome.Diagnostics, diag.Warningf(
			DeclaredEdgeDiagnosticCode, wsproto.WorkspaceConfigFilename,
			"%v; the declared-edge check ran in %q", err, mode))
	}
	findings := DeclaredEdgeDiagnostics(req.Workspace, mode)
	if len(findings) == 0 {
		return outcome, nil
	}
	if mode == DeclaredEdgesModeEnforce {
		return nil, declaredEdgesRefusal(findings)
	}
	outcome.Diagnostics = append(outcome.Diagnostics, findings...)
	return outcome, nil
}

// checkVisibility holds the synchronized graph to the import boundaries its
// projects declare.
//
// It runs HERE and not in the loader because this is the first point where the
// graph is the real one: synchronize adopted the merged provider view, so every
// edge carries the manifest family it came from, and a declared edge with no
// import behind it is finally distinguishable from an import. It runs after the
// snapshot has been written for the same reason the repair commands survive a
// refusal — a refused run must not also cost the workspace its index, or the
// next command re-probes from scratch while the tree never moved.
func checkVisibility(req SyncRequest, outcome *SyncOutcome) (*SyncOutcome, error) {
	var config *wsproto.Config
	if req.Workspace != nil {
		config = req.Workspace.Config
	}
	mode, err := ResolveVisibilityMode(config)
	var unreadable []diag.Diagnostic
	if err != nil {
		// An unreadable switch is reported and the check falls back to its
		// default rather than failing on the switch alone: the document that
		// carries the typo is the one a user edits to fix it. A refusal the
		// fallback causes carries the same warning, so the run names both.
		unreadable = append(unreadable, diag.Warningf(
			VisibilityModeInvalidCode, wsproto.WorkspaceConfigFilename,
			"%v; the visibility check ran in %q", err, mode))
		outcome.Diagnostics = append(outcome.Diagnostics, unreadable...)
	}
	findings := VisibilityFindings(req.Workspace, mode)
	if len(findings) == 0 {
		return outcome, nil
	}
	if mode == VisibilityModeEnforce {
		return nil, visibilityRefusal(findings, declaresVisibilityMode(config), unreadable...)
	}
	outcome.Diagnostics = append(outcome.Diagnostics, findings...)
	return outcome, nil
}

func synchronize(req SyncRequest) (*SyncOutcome, error) {
	if req.Workspace == nil {
		return nil, errors.New("workspace: cannot synchronize a nil workspace")
	}
	root := req.Workspace.Root
	outcome := &SyncOutcome{Merged: map[string]wsproto.MergedProject{}}
	if req.Policy.Persists() {
		release, err := acquireWorkspaceSyncLock(req.Context, root, req.OnLockWait)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			// An inaccessible lock path is a read-only workspace, not a reason to
			// discard provider facts. Continue in memory, but make persistence
			// impossible: writing without the cross-process exclusion would trade a
			// recoverable warning for a corrupt or lost concurrent update.
			outcome.Diagnostics = append(outcome.Diagnostics, diag.Warningf(
				extproto.FailureWorkspaceSnapshotInvalid, WorkspaceIndexFilename,
				"workspace index lock is unavailable; continuing with an in-memory view and no snapshot write: %v", err))
			req.Policy.Plan = true
		} else {
			defer release()
		}
	}

	bindings := sortedBindings(req.Providers)
	existing, loadErr := LoadSnapshot(root)
	if loadErr != nil {
		// A corrupt index is a recoverable state, not a fatal one: the next
		// write repairs it. Reporting it is what keeps a silently rebuilt
		// snapshot from hiding a disk problem.
		outcome.Diagnostics = append(outcome.Diagnostics, diag.Warningf(
			extproto.FailureWorkspaceSnapshotInvalid, WorkspaceIndexFilename,
			"workspace snapshot could not be read and will be rebuilt: %v", loadErr))
		existing = nil
	}

	if len(bindings) == 0 {
		// No adapter-declaring extension resolved for this run. That is TWO
		// different situations and they must not be treated alike:
		//
		//   - The workspace genuinely has no providers. Its index is still core's
		//     own, and `projects sync` is still the command that writes it, so it
		//     is rebuilt and persisted.
		//   - The providers could not be resolved THIS TIME — a fresh worktree,
		//     an extension whose artifact is not materialized yet, a run that
		//     failed discovery. Overwriting a snapshot that carries recorded
		//     provider answers with one that carries none would delete every
		//     project's language identity and dependency edges from the index,
		//     and the NEXT load — which adopts whatever the index says — would
		//     resolve a different workspace and therefore different cache keys
		//     for an unchanged tree.
		//
		// The recorded answers are the tiebreaker: a snapshot that has them is
		// left exactly as it is — and SAYS so, because "the index was kept
		// because nobody answered for it" and "the index is current" are the same
		// silence otherwise.
		if existing != nil && len(existing.Providers) > 0 {
			outcome.Diagnostics = append(outcome.Diagnostics, diag.Warningf(
				extproto.FailureWorkspaceSnapshotInvalid, WorkspaceIndexFilename,
				"no workspace adapter resolved for this run, but %s records %d provider answer(s) (%s); "+
					"the recorded answers were kept rather than overwritten with an empty view",
				WorkspaceIndexFilename, len(existing.Providers),
				strings.Join(snapshotProviderNames(existing), ", ")))
			outcome.Snapshot = existing
			outcome.Fresh = true
			outcome.Merged = adoptRecordedView(req.Workspace, existing, outcome)
			return outcome, nil
		}
		return finishSync(root, req, outcome, existing, NewSnapshot(req.Workspace, req.Workspace.ProbeDigest())), nil
	}

	candidates := req.CandidatePaths
	if len(candidates) == 0 {
		candidates = workspaceCandidatePaths(req.Workspace)
	}

	plan := planProbe(root, existing, bindings, req.Full)
	outcome.Reason, outcome.Changed = plan.reason, plan.changed
	if plan.fresh {
		outcome.Snapshot = existing
		outcome.Fresh = true
		// Adopt even though Load already did: this is the ONE place the two
		// paths are made to agree by construction rather than by inspection. A
		// zero-process load and a re-probing load run the same adoption over the
		// same merge, so "did this run probe?" cannot change what the workspace
		// resolves to — or what its cache keys are.
		outcome.Merged = adoptRecordedView(req.Workspace, existing, outcome)
		return outcome, nil
	}

	results := make([]wsproto.ProbeResult, 0, len(bindings))
	type probeSlot struct {
		outcome *ProbeOutcome
		err     error
	}
	slots := make([]probeSlot, len(bindings))
	stale := make([]int, 0, len(bindings))
	for i, binding := range bindings {
		if !plan.stale[binding.Scope.Extension] {
			if carried, ok := recordedResult(existing, binding.Scope.Extension); ok {
				slots[i].outcome = &ProbeOutcome{
					Results:     []wsproto.ProbeResult{carried},
					Diagnostics: carried.Diagnostics,
				}
				continue
			}
		}
		stale = append(stale, i)
	}

	// Providers own disjoint reads and prepare their runtimes through the
	// content-addressed runtime store, whose per-digest lock keeps the mutation
	// exclusive across processes. Run the independent chains with a small bound,
	// but record into name-sorted slots: merge bytes, diagnostics and the first
	// reported failure therefore remain independent of completion order.
	workers := probeWorkerCount(len(stale))
	if workers > 0 {
		work := make(chan int)
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					binding := bindings[i]
					slots[i].outcome, slots[i].err = RunProbe([]ProbeProvider{binding.Provider}, wsproto.ProbeRequest{
						Version:   wsproto.ProbeProtocolVersion,
						Extension: binding.Scope.Extension,
						Reason:    req.Reason,
						Paths:     candidates,
						// Asked only of an adapter that declared it: an
						// older provider strict-decodes this request.
						DependencySources: binding.Scope.DependencySources,
					}, nil)
				}
			}()
		}
		for _, i := range stale {
			work <- i
		}
		close(work)
		wg.Wait()
	}

	for i, binding := range bindings {
		single, err := slots[i].outcome, slots[i].err
		if err != nil {
			return nil, err
		}
		if single == nil {
			continue
		}
		if plan.stale[binding.Scope.Extension] {
			outcome.Probed = append(outcome.Probed, binding.Scope.Extension)
		}
		// A carried-forward answer carries its recorded findings; a provider
		// that ran contributes only its fresh findings. Iterating the shared
		// slots here keeps both kinds in extension-name order.
		outcome.Diagnostics = append(outcome.Diagnostics, single.Diagnostics...)
		results = append(results, single.Results...)
	}

	// A recorded provider that did not resolve for THIS run keeps its answer.
	//
	// The providers loop above ranges over the CURRENT bindings only, so an
	// extension whose artifact vanished from the gitignored `.putnami/`, whose
	// `deps install` was interrupted, or whose runtime was garbage-collected
	// simply stops appearing — and because snapshotStillHolds treats a shorter
	// provider list as a reason to WRITE, the rebuilt index would delete its
	// recorded answer. Every project only that provider described would revert to
	// authored-config-only identity: no source name, no type, no metadata, and no
	// provider-derived dependency edges, so `--impacted` would compute over an
	// incomplete graph while the tree never changed. The metadata digests and the
	// workspace IdentityDigest would move, silently.
	//
	// This is the per-provider form of the all-or-nothing refusal above, and it
	// takes the same position: the recorded answer is evidence, and a run that
	// cannot produce a better one keeps it and SAYS so. The carry-forward has to
	// reach the merge as well as the snapshot — otherwise this run's adopted view
	// would lack those projects while the persisted index still recorded them, and
	// the next load would resolve a different workspace than the run that wrote it.
	orphaned := orphanedProviders(existing, bindings)
	for _, provider := range orphaned {
		results = append(results, provider.Result)
		outcome.Diagnostics = append(outcome.Diagnostics, provider.Result.Diagnostics...)
	}
	if len(orphaned) > 0 {
		names := make([]string, 0, len(orphaned))
		for _, provider := range orphaned {
			names = append(names, provider.Extension)
		}
		outcome.Diagnostics = append(outcome.Diagnostics, diag.Warningf(
			extproto.FailureWorkspaceSnapshotInvalid, WorkspaceIndexFilename,
			"%d provider(s) recorded in %s did not resolve for this run (%s); "+
				"their recorded answers were kept rather than dropped, so the projects only they describe "+
				"keep their identity and dependency edges",
			len(names), WorkspaceIndexFilename, strings.Join(names, ", ")))
	}
	sortResultsByExtension(results)

	merged, mergeDiags := wsproto.MergeProbeResults(results, explicitProjects(req.Workspace))
	if diag.HasErrors(mergeDiags) {
		failure := wsproto.NewProbeFailure(wsproto.ProbeFailureConflict, "",
			"providers reported irreconcilable project metadata")
		failure.Diagnostics = diag.Errors(mergeDiags)
		return nil, failure
	}
	outcome.Merged = merged
	outcome.Diagnostics = append(outcome.Diagnostics, mergeDiags...)

	// Adopt BEFORE the snapshot is built. The snapshot records each project's
	// metadata digest, and that digest is computed from the project's resolved
	// identity — so a snapshot taken before adoption would record the identity
	// of the workspace as it was BEFORE this probe answered, and every later
	// load would compare the tree against a key nothing produces.
	//
	// And adoption is VALIDATED, which is what makes this the point of no
	// persistence for a poisoned view: a merged answer that resolves two
	// projects to one name, or that closes a dependency cycle, is a workspace
	// Load refuses — so writing it into the index would take the workspace down
	// on the NEXT command and take every repair path with it. The run that
	// created it is the only one that can still report what happened, so it
	// fails here, typed, and nothing reaches disk.
	if err := req.Workspace.AdoptValidatedProbeView(merged); err != nil {
		failure := wsproto.NewProbeFailure(wsproto.ProbeFailureConflict, "",
			"the merged provider view does not describe a loadable workspace: %v", err)
		failure.Diagnostics = append(failure.Diagnostics, diag.Errorf(
			extproto.FailureWorkspaceSnapshotInvalid, WorkspaceIndexFilename,
			"the index was NOT updated; %s still records the last view that loaded", WorkspaceIndexFilename))
		return nil, failure
	}

	providers := make([]SnapshotProvider, 0, len(bindings)+len(orphaned))
	for _, binding := range bindings {
		result, ok := resultFor(results, binding.Scope.Extension)
		if !ok {
			continue
		}
		providers = append(providers, SnapshotProvider{
			Extension:      binding.Scope.Extension,
			Digest:         wsproto.ProbeResultDigest(result),
			Result:         result,
			Inputs:         providerInputs(root, candidates, binding.Scope.Inputs),
			Implementation: binding.Implementation,
		})
	}
	// The orphans travel VERBATIM — result, digest and recorded inputs. The
	// inputs especially: this run has no scope to re-resolve them from (the
	// binding that declared them is what went missing), and recording an empty
	// input set would make the snapshot permanently "valid" across any later
	// change to the files that provider actually reads.
	providers = append(providers, orphaned...)
	sort.SliceStable(providers, func(i, j int) bool { return providers[i].Extension < providers[j].Extension })

	return finishSync(root, req, outcome,
		existing, NewSnapshotWithProviders(req.Workspace, aggregateProbeDigest(req.Workspace, results), providers)), nil
}

const maxParallelWorkspaceProbes = 4

func probeWorkerCount(total int) int {
	if total <= 0 {
		return 0
	}
	workers := min(total, maxParallelWorkspaceProbes, runtime.GOMAXPROCS(0))
	return max(workers, 1)
}

// finishSync persists the rebuilt snapshot, unless the recorded one already
// says the same thing.
//
// Skipping an equivalent write is not an optimization: rewriting the index on
// every sync would move its modification time, and a workspace's tooling has no
// way to tell "the index was refreshed" from "the workspace changed". The
// comparison is over CONTENT identity (the digests) plus a full re-hash of the
// recorded inputs — never over the file's own timestamps.
func finishSync(root string, req SyncRequest, outcome *SyncOutcome, existing, rebuilt *Snapshot) *SyncOutcome {
	if snapshotStillHolds(root, existing, rebuilt) {
		outcome.Snapshot = existing
		outcome.Fresh = true
		return outcome
	}
	outcome.Snapshot = rebuilt
	if err := WriteSnapshot(root, rebuilt, req.Policy); err != nil {
		if !errors.Is(err, ErrSnapshotWriteNotPermitted) {
			outcome.Diagnostics = append(outcome.Diagnostics, diag.Warningf(
				extproto.FailureWorkspaceSnapshotInvalid, WorkspaceIndexFilename,
				"workspace snapshot could not be written: %v", err))
		}
		return outcome
	}
	outcome.Persisted = true
	return outcome
}

// snapshotStillHolds reports whether the recorded snapshot already describes
// what the rebuild resolved. Every half is required: the identity and probe
// digests answer "is this the same resolution", the provider digests answer "is
// it the same provider answer", and re-hashing the recorded inputs answers
// "does that resolution still match the tree".
func snapshotStillHolds(root string, existing, rebuilt *Snapshot) bool {
	if existing == nil || rebuilt == nil {
		return false
	}
	if existing.IdentityDigest != rebuilt.IdentityDigest || existing.ProbeDigest != rebuilt.ProbeDigest {
		return false
	}
	if len(existing.Providers) != len(rebuilt.Providers) {
		return false
	}
	for i := range existing.Providers {
		if existing.Providers[i].Extension != rebuilt.Providers[i].Extension ||
			existing.Providers[i].Digest != rebuilt.Providers[i].Digest ||
			existing.Providers[i].Implementation != rebuilt.Providers[i].Implementation {
			return false
		}
	}
	return existing.Validate(root).Valid
}

// probePlan is the snapshot-first decision.
type probePlan struct {
	fresh   bool
	reason  string
	changed []string
	stale   map[string]bool
}

// planProbe decides which providers must be asked.
//
// The attribution rule is the whole reason "one changed manifest starts exactly
// one probe" is achievable: a changed input is charged to the providers whose
// declared input patterns claim it, and a changed input NO provider claims —
// core's own putnami.json, the workspace config, a scope config — invalidates
// all of them, because those files decide membership and explicit identity.
func planProbe(root string, existing *Snapshot, bindings []ProviderBinding, full bool) probePlan {
	plan := probePlan{stale: make(map[string]bool, len(bindings))}
	markAll := func(reason string) probePlan {
		plan.reason = reason
		for _, binding := range bindings {
			plan.stale[binding.Scope.Extension] = true
		}
		return plan
	}

	if full {
		return markAll("full scan requested")
	}
	if existing == nil {
		return markAll("no workspace snapshot")
	}
	for _, binding := range bindings {
		recorded, ok := recordedProvider(existing, binding.Scope.Extension)
		if !ok {
			return markAll(fmt.Sprintf("no recorded answer for %s", binding.Scope.Extension))
		}
		// A recorded answer describes the implementation that produced it.
		// The same inputs read by a different implementation are a different
		// answer, so the provider is asked again; a record with no identity
		// predates the field and is asked once.
		if implementationMoved(recorded.Implementation, binding.Implementation) {
			plan.stale[binding.Scope.Extension] = true
			plan.reason = fmt.Sprintf("provider %s implementation changed", binding.Scope.Extension)
		}
	}

	validity := existing.Validate(root)
	if validity.Valid {
		plan.fresh = len(plan.stale) == 0
		return plan
	}
	plan.changed = validity.Changed
	plan.reason = validity.Reason

	for _, changed := range validity.Changed {
		claimed := false
		for _, binding := range bindings {
			if binding.Scope.MatchesInput(changed) {
				plan.stale[binding.Scope.Extension] = true
				claimed = true
			}
		}
		if !claimed {
			return markAll(validity.Reason)
		}
	}
	return plan
}

// sortedBindings orders providers by extension name so the probe order, the
// snapshot's provider order and the diagnostics order never depend on the order
// extensions were discovered in.
func sortedBindings(bindings []ProviderBinding) []ProviderBinding {
	ordered := make([]ProviderBinding, 0, len(bindings))
	seen := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		name := binding.Scope.Extension
		if name == "" || binding.Provider == nil || seen[name] {
			continue
		}
		seen[name] = true
		ordered = append(ordered, binding)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Scope.Extension < ordered[j].Scope.Extension })
	return ordered
}

// orphanedProviders names the providers the snapshot RECORDS an answer for and
// this run has no binding for, in extension-name order.
//
// "No binding" is the observable fact; the reasons behind it are all the same
// shape — an extension artifact that is not materialized in a fresh worktree, an
// interrupted `deps install`, a garbage-collected runtime, a temporarily removed
// config entry. None of them is evidence that the provider's last answer became
// wrong, so none of them may be a reason to delete it.
func orphanedProviders(existing *Snapshot, bindings []ProviderBinding) []SnapshotProvider {
	if existing == nil || len(existing.Providers) == 0 {
		return nil
	}
	bound := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		bound[binding.Scope.Extension] = true
	}
	orphaned := make([]SnapshotProvider, 0, len(existing.Providers))
	for _, provider := range existing.Providers {
		if bound[provider.Extension] {
			continue
		}
		orphaned = append(orphaned, provider)
	}
	sort.SliceStable(orphaned, func(i, j int) bool { return orphaned[i].Extension < orphaned[j].Extension })
	return orphaned
}

// sortResultsByExtension keeps the merge input in one deterministic order.
//
// It matters beyond tidiness: the aggregate probe digest is folded over this
// slice, and carrying an orphaned provider's answer forward appends it after the
// bound ones — so without the sort the recorded digest would depend on WHICH
// providers happened to resolve this run rather than on what they answered.
func sortResultsByExtension(results []wsproto.ProbeResult) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Extension < results[j].Extension })
}

// recordedProvider returns the snapshot's record for one extension.
func recordedProvider(snapshot *Snapshot, extension string) (SnapshotProvider, bool) {
	if snapshot == nil {
		return SnapshotProvider{}, false
	}
	for _, provider := range snapshot.Providers {
		if provider.Extension == extension {
			return provider, true
		}
	}
	return SnapshotProvider{}, false
}

// implementationMoved reports whether a recorded provider answer was produced
// by an implementation other than the one bound now. An unknown bound identity
// compares equal to anything: re-probing on ignorance would cost a provider
// start on every command.
func implementationMoved(recorded, bound string) bool {
	return bound != "" && recorded != bound
}

func recordedResult(snapshot *Snapshot, extension string) (wsproto.ProbeResult, bool) {
	if snapshot == nil {
		return wsproto.ProbeResult{}, false
	}
	for _, provider := range snapshot.Providers {
		if provider.Extension == extension {
			return provider.Result, true
		}
	}
	return wsproto.ProbeResult{}, false
}

func resultFor(results []wsproto.ProbeResult, extension string) (wsproto.ProbeResult, bool) {
	for _, result := range results {
		if result.Extension == extension {
			return result, true
		}
	}
	return wsproto.ProbeResult{}, false
}

// adoptRecordedView replays a RECORDED snapshot onto the workspace and returns
// the view that was actually adopted.
//
// A recorded view gets the same validation a freshly probed one does, and for
// the same reason: it is the view Load will adopt on the next command, so if
// this run cannot use it, saying so now — with the file named — is the
// difference between a repairable workspace and one whose only escape is
// deleting a file no diagnostic mentions. A rejected view degrades to
// the empty one, which is the identity a fresh checkout has.
//
// The recorded FINDINGS are replayed alongside the recorded view, which is what
// makes the invalid-manifest warning an ongoing statement about the tree
// rather than a one-time announcement. A provider's `invalid-manifest`
// diagnostic is persisted inside SnapshotProvider.Result.Diagnostics, but
// mergeRecordedResults drops it — so on the zero-probe path the outcome carried
// nothing, and a project whose package.json was corrupted printed the warning on
// the run that first saw it and never again, while staying degraded and renamed
// to its directory basename on every later run. engine/workspace.go still
// promises that warning by name.
func adoptRecordedView(ws *Workspace, snapshot *Snapshot, outcome *SyncOutcome) map[string]wsproto.MergedProject {
	outcome.Diagnostics = append(outcome.Diagnostics, replayRecordedDiagnostics(snapshot)...)
	merged := mergeRecordedResults(snapshot, ws)
	if len(merged) == 0 {
		ws.AdoptProbeViewUnvalidated(merged)
		return merged
	}
	if err := ws.AdoptValidatedProbeView(merged); err != nil {
		outcome.Diagnostics = append(outcome.Diagnostics, diag.Warningf(
			extproto.FailureWorkspaceSnapshotInvalid, WorkspaceIndexFilename,
			"%s records a provider view that does not describe a loadable workspace (%v); "+
				"it was not adopted — run `putnami projects sync` to rebuild the index",
			WorkspaceIndexFilename, err))
		empty := map[string]wsproto.MergedProject{}
		ws.AdoptProbeViewUnvalidated(empty)
		return empty
	}
	return merged
}

// replayRecordedDiagnostics returns every finding the snapshot's recorded
// provider answers carry, in the snapshot's own (extension-name) order.
//
// This is only ever called on the paths where NO provider ran for the answers in
// question — the zero-probe fresh path and the no-bindings path. A provider that
// actually probed this run contributes its fresh diagnostics from the results
// loop instead, and must not also contribute its recorded copy, or every probing
// run would print each finding twice.
func replayRecordedDiagnostics(snapshot *Snapshot) []diag.Diagnostic {
	if snapshot == nil {
		return nil
	}
	var replayed []diag.Diagnostic
	for _, provider := range snapshot.Providers {
		replayed = append(replayed, provider.Result.Diagnostics...)
	}
	return replayed
}

// snapshotProviderNames lists the extensions a snapshot recorded an answer for,
// in the snapshot's own (extension-name) order.
func snapshotProviderNames(snapshot *Snapshot) []string {
	if snapshot == nil {
		return nil
	}
	names := make([]string, 0, len(snapshot.Providers))
	for _, provider := range snapshot.Providers {
		names = append(names, provider.Extension)
	}
	return names
}

// mergeRecordedResults rebuilds the merged view from a validated snapshot, so a
// zero-process load produces the SAME view a probing load would. Without it,
// "the snapshot is fresh" would mean "the provider view is missing", and a
// consumer's behavior would depend on whether a probe happened to run.
func mergeRecordedResults(snapshot *Snapshot, ws *Workspace) map[string]wsproto.MergedProject {
	if snapshot == nil || len(snapshot.Providers) == 0 {
		return map[string]wsproto.MergedProject{}
	}
	results := make([]wsproto.ProbeResult, 0, len(snapshot.Providers))
	for _, provider := range snapshot.Providers {
		results = append(results, provider.Result)
	}
	merged, _ := wsproto.MergeProbeResults(results, explicitProjects(ws))
	return merged
}

// aggregateProbeDigest folds core's own view together with every provider's, so
// the recorded digest describes the whole answer rather than core's half.
func aggregateProbeDigest(ws *Workspace, results []wsproto.ProbeResult) string {
	all := make([]wsproto.ProbeResult, 0, len(results)+1)
	all = append(all, ws.ProbeResultView())
	all = append(all, results...)
	return wsproto.ProbeWorkspaceDigest(all)
}

// workspaceCandidatePaths is the bounded candidate set an ordinary load asks
// about: the workspace root plus every resolved project directory. Discovering
// a directory that is not yet a member is `projects sync`'s job — it is the
// command that walks the tree — so an ordinary load never pays for a scan.
func workspaceCandidatePaths(ws *Workspace) []string {
	paths := make([]string, 0, len(ws.Projects)+1)
	paths = append(paths, wsproto.ProbeRootPath)
	for _, project := range ws.Projects {
		paths = append(paths, probePathOf(project.Path))
	}
	sort.Strings(paths)
	return dedupeSorted(paths)
}

func dedupeSorted(values []string) []string {
	out := values[:0]
	previous := ""
	for i, value := range values {
		if i > 0 && value == previous {
			continue
		}
		previous = value
		out = append(out, value)
	}
	return out
}

// explicitProjects projects each resolved project's EXPLICIT configuration onto
// the merge input.
//
// Only putnami.json values count as explicit. That is the merge rule, and it is
// load-bearing here: feeding core's already-resolved name back in as "explicit"
// would make every probe answer agree with core by construction, and the
// divergence this slice exists to detect would be invisible.
func explicitProjects(ws *Workspace) map[string]wsproto.ExplicitProject {
	explicit := make(map[string]wsproto.ExplicitProject, len(ws.Projects))
	for _, project := range ws.Projects {
		if project.Config == nil {
			continue
		}
		explicit[probePathOf(project.Path)] = wsproto.ExplicitProject{
			Name:         project.Config.Name,
			Type:         project.Config.Type,
			Tags:         project.Config.Tags,
			Dependencies: project.Config.Dependencies,
			Publish:      project.Config.Publish,
			RunsWith:     project.Config.RunsWith,
			Extensions:   project.Config.Extensions,
		}
	}
	return explicit
}
