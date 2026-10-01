package agentctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// Agent-content lifecycle: the wiring between the workspace's opt-in, the
// extension release the lock pins, and the materializer that owns the file
// rules (internal/agentartifacts, ADR 0004). This file adds NO ownership rule
// of its own; it holds the two guarantees the materializer cannot hold alone,
// because both are about the RUN rather than one contribution (ADR 0047 §4):
//
//   - every opted-in contribution is planned before any of them is written, so
//     one collision aborts the whole run rather than the tail of it;
//   - an ownership record moves only after its files were published.
//
// The only opt-in is `extension:<name>` in `agentArtifacts`
// (agent_workflows_extension.go): the content is the one the declared
// extension ships, its version is the extension's, and its pin is the
// extension's own lock entry. A workspace that declares no opt-in is untouched
// by every pass, which is what makes `agentArtifacts` an opt-in rather than a
// default.
//
// An entry in the forms earlier releases installed — a registry artifact
// (`name` or `name:constraint`) or an in-tree project (`/path`) — is refused by
// every pass with nothing written. `putnami migrate agent-content` is the only
// reader of those entries (agent_content_legacy.go).

// AgentArtifactRef names one owner of agent files: an `extension:<name>`
// opt-in, or, inside the migration and the overlap checks, an ownership record.
type AgentArtifactRef struct {
	// Name is the owner's identity. It also names the ownership record under
	// .putnami/agent-artifacts/.
	Name string
	// Extension is the declared extension whose agent content an opt-in
	// selects. It equals Name for an opt-in, and is empty for a record that no
	// opt-in declares.
	Extension string
}

// AgentArtifactResolution is one verified content tree together with the exact
// pin that describes it. Holding one is the proof that a version was resolved
// and its bytes were checked; nothing downstream re-derives either.
type AgentArtifactResolution struct {
	// Entry is the exact pin: version, identity digest, content manifest
	// digest.
	Entry lockfile.AgentArtifactLockEntry
	// Dir is the verified tree.
	Dir string
}

// preparedAgentArtifact is one contribution carried to the last point before a
// write: bytes verified, ownership state read, plan built. Collecting every
// opted-in contribution in this state BEFORE applying any of them is what
// makes a collision abort the run instead of the remainder of it.
type preparedAgentArtifact struct {
	ref      AgentArtifactRef
	artifact *agentartifacts.Artifact
	plan     *agentartifacts.Plan
}

// DeclaredAgentArtifacts parses the workspace's opt-ins, sorted by name so
// repeated runs act in the same order. An empty result means the workspace
// never asked for agent content, which every caller treats as "leave this
// workspace alone" rather than as an error.
//
// A repeated opt-in is dropped. An entry in a form this CLI no longer installs
// fails the whole declaration and names the migration that moves it to an
// extension, or the unfinished migration that must complete first.
func DeclaredAgentArtifacts(wsRoot string, cfg *wsproto.Config) ([]AgentArtifactRef, error) {
	if cfg == nil {
		return nil, nil
	}
	seen := make(map[string]bool, len(cfg.AgentArtifacts))
	refs := make([]AgentArtifactRef, 0, len(cfg.AgentArtifacts))
	var legacy []string
	for _, declared := range cfg.AgentArtifacts {
		declared = strings.TrimSpace(declared)
		if declared == "" {
			continue
		}
		ref, ok, err := parseExtensionAgentContentReference(declared)
		if !ok {
			legacy = append(legacy, declared)
			continue
		}
		if err != nil {
			return nil, err
		}
		if seen[ref.Name] {
			continue
		}
		seen[ref.Name] = true
		refs = append(refs, ref)
	}
	if len(legacy) > 0 {
		// A migration that stopped partway leaves legacy entries behind; only
		// finishing or rolling it back resolves that, so it is named first.
		if err := requireNoUnfinishedAgentContentMigration(wsRoot); err != nil {
			return nil, err
		}
		return nil, legacyAgentArtifactDeclarationsError(wsRoot, cfg, legacy)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// localContentProbeVersion is the version the first of two builds of a local
// extension's content uses. Its bytes are discarded: it exists only to learn
// the file digests the real version is derived from.
const localContentProbeVersion = "0.0.0-local"

// localContentVersion derives a local build's version from the emitted files —
// "0.0.0-local-" plus twelve hex characters of their digest — so an unchanged
// source rebuilds into the same directory and the ownership record names a
// version that changes exactly when the content does.
func localContentVersion(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	var listing strings.Builder
	for _, name := range paths {
		listing.WriteString(name)
		listing.WriteByte(0)
		listing.WriteString(lockfile.HashBytes(files[name]))
		listing.WriteByte('\n')
	}
	return localContentProbeVersion + "-" + lockfile.HashBytes([]byte(listing.String()))[:12]
}

// stageLocalContent writes a built tree beside its final directory and renames
// it into place, so a reader never sees a half-written tree. A tree already at
// dir that failed verification is replaced.
func stageLocalContent(dir string, result *sdkagentartifact.Result) error {
	staging := fmt.Sprintf("%s.staging-%d", dir, os.Getpid())
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	members := make(map[string][]byte, len(result.Files)+1)
	for name, content := range result.Files {
		if !wsproto.ValidAgentArtifactPath(name) {
			return fmt.Errorf("built content declares unsafe path %q", name)
		}
		members[name] = content
	}
	members[wsproto.AgentArtifactManifestFilename] = result.Manifest
	for name, content := range members {
		target := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	return os.Rename(staging, dir)
}

// MaterializeLocalAgentContent materializes the agent content of the opted-in
// local extensions: the ones this workspace declares by path and builds from
// its own sources. It is what `putnami context generate` runs so an author who
// edited the source regenerates the workspace copy with the same command that
// regenerates the rest of the generated guidance. It resolves nothing and never
// touches the lock. It reports whether any file changed.
func MaterializeLocalAgentContent(wsRoot string, cfg *wsproto.Config, out io.Writer) (bool, error) {
	refs, err := DeclaredAgentArtifacts(wsRoot, cfg)
	if err != nil {
		return false, err
	}
	if len(refs) == 0 {
		return false, nil
	}
	if err := requireNoUnfinishedAgentContentMigration(wsRoot); err != nil {
		return false, err
	}
	prepared := make([]preparedAgentArtifact, 0, len(refs))
	for _, ref := range refs {
		local, err := localExtensionAgentContent(wsRoot, cfg, ref)
		if err != nil {
			return false, err
		}
		if !local {
			continue
		}
		item, err := prepareExtensionAgentContent(wsRoot, cfg, ref)
		if err != nil {
			return false, err
		}
		prepared = append(prepared, item)
	}
	if len(prepared) == 0 {
		return false, nil
	}
	changed := false
	for _, item := range prepared {
		if agentArtifactPlanChanges(item.plan) {
			changed = true
		}
	}
	if err := commitAgentArtifacts(wsRoot, prepared, retiredAgentArtifacts{}, out); err != nil {
		return false, err
	}
	return changed, nil
}

func agentArtifactPlanChanges(plan *agentartifacts.Plan) bool {
	for _, entry := range plan.Entries {
		if entry.Action != agentartifacts.ActionUnchanged {
			return true
		}
	}
	return false
}

// InstallAgentWorkflows materializes the agent content of every opted-in
// extension at the release the COMMITTED lock pins. It resolves nothing and
// writes no lock entry, so running it twice over an unchanged workspace changes
// not one byte.
func InstallAgentWorkflows(ctx context.Context, wsRoot string, cfg *wsproto.Config, out io.Writer) error {
	_, err := InstallAgentWorkflowsWithResult(ctx, wsRoot, cfg, out)
	return err
}

// AgentWorkflowInstallAction identifies one contribution whose workspace files
// changed during a successful install.
type AgentWorkflowInstallAction struct {
	Name    string
	Version string
}

// InstallAgentWorkflowsWithResult performs the lock-authoritative install and
// returns only contributions whose plan created, updated, or removed files. The
// result lets a composed lifecycle omit verified no-ops without inspecting the
// filesystem or parsing human output.
func InstallAgentWorkflowsWithResult(_ context.Context, wsRoot string, cfg *wsproto.Config, out io.Writer) ([]AgentWorkflowInstallAction, error) {
	refs, err := DeclaredAgentArtifacts(wsRoot, cfg)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if err := requireNoUnfinishedAgentContentMigration(wsRoot); err != nil {
		return nil, err
	}
	prepared := make([]preparedAgentArtifact, 0, len(refs))
	for _, ref := range refs {
		// The extension's own pin was installed by the extensions phase; its
		// content is read from that exact release, never resolved.
		item, err := prepareExtensionAgentContent(wsRoot, cfg, ref)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, item)
	}
	actions := make([]AgentWorkflowInstallAction, 0, len(prepared))
	for _, item := range prepared {
		if agentArtifactPlanChanges(item.plan) {
			actions = append(actions, AgentWorkflowInstallAction{Name: item.ref.Name, Version: item.plan.Version})
		}
	}
	if err := commitAgentArtifacts(wsRoot, prepared, retiredAgentArtifacts{}, out); err != nil {
		return nil, err
	}
	return actions, nil
}

// AdoptAgentWorkflows is the agent phase of `putnami upgrade`: it retires what
// the workspace no longer opts into, then materializes every opted-in
// contribution at the release the extensions phase of the same command moved
// the extension to.
//
// The ordering is the guarantee: verify every contribution (nothing in the
// workspace has been written yet) → plan every contribution → abort if ANY of
// them collides → apply → record ownership. A failure before the last step
// leaves the previous files and records as they were.
func AdoptAgentWorkflows(_ context.Context, wsRoot string, cfg *wsproto.Config, out io.Writer) error {
	refs, err := DeclaredAgentArtifacts(wsRoot, cfg)
	if err != nil {
		return err
	}
	if err := requireNoUnfinishedAgentContentMigration(wsRoot); err != nil {
		return err
	}
	if len(refs) == 0 {
		// Nothing to adopt, but an opt-in the user removed still has files to
		// retire.
		retired, err := planUndeclaredAgentArtifacts(wsRoot, nil)
		if err != nil {
			return err
		}
		released, err := applyAgentArtifactRemovals(wsRoot, retired)
		if err != nil {
			return err
		}
		return commitAgentArtifacts(wsRoot, nil, retiredAgentArtifacts{items: retired, released: released}, out)
	}
	// Content that supersedes an artifact this clone still records is refused
	// here, before retirement removes a file: retiring the superseded record
	// and then refusing the content would leave the workspace with neither.
	for _, ref := range refs {
		source, err := extension.LocateAgentContent(wsRoot, cfg, ref.Extension)
		if err != nil {
			return agentArtifactFailure(ref.Name, nil, err)
		}
		if err := refuseSupersededOwners(wsRoot, source); err != nil {
			return err
		}
	}
	retired, err := planUndeclaredAgentArtifacts(wsRoot, refs)
	if err != nil {
		return err
	}
	// Every opted-in contribution is resolved, verified and planned before
	// retirement removes a file. The extension version moved in the extensions
	// phase of the same command; its content follows that pin and adds none of
	// its own.
	prepared := make([]preparedAgentArtifact, 0, len(refs))
	for _, ref := range refs {
		item, err := prepareExtensionAgentContent(wsRoot, cfg, ref)
		if err != nil {
			return err
		}
		prepared = append(prepared, item)
	}
	if err := rejectCollisionsRetirementKeeps(prepared, retired); err != nil {
		return err
	}
	if err := rejectAgentArtifactTargetOverlaps(prepared); err != nil {
		return err
	}
	// Retirement, then the authoritative plans. A removal plan is classified
	// against the retiring record, and a declared plan against what is on
	// disk: the plans made above disagree with the removal the moment an owner
	// is renamed, because the old record still owns the paths the new name is
	// about to claim. Planning again after the removal classifies a clean tree.
	released, err := applyAgentArtifactRemovals(wsRoot, retired)
	if err != nil {
		return err
	}
	for i := range prepared {
		if prepared[i].plan, err = replanAgentArtifact(wsRoot, prepared[i]); err != nil {
			return err
		}
	}
	return commitAgentArtifacts(wsRoot, prepared, retiredAgentArtifacts{items: retired, released: released}, out)
}

// rejectCollisionsRetirementKeeps refuses, before retirement removes a file,
// every collision in the opted-in plans that retirement does not clear. A
// collision on a path a retiring owner removes disappears with the removal;
// any other one — an unmanaged file, a user edit, a file retirement releases
// because the user edited it — still blocks the run after it, so it blocks
// the run now, with nothing written.
func rejectCollisionsRetirementKeeps(prepared []preparedAgentArtifact, retired []retiredAgentArtifact) error {
	removed := map[string]bool{}
	for _, item := range retired {
		if item.plan == nil {
			continue
		}
		for _, entry := range item.plan.Select(agentartifacts.ActionRemove) {
			removed[entry.Path] = true
		}
	}
	kept := make([]preparedAgentArtifact, 0, len(prepared))
	for _, item := range prepared {
		plan := *item.plan
		plan.Entries = nil
		for _, entry := range item.plan.Entries {
			if entry.Action == agentartifacts.ActionCollide && removed[entry.Path] {
				continue
			}
			plan.Entries = append(plan.Entries, entry)
		}
		kept = append(kept, preparedAgentArtifact{ref: item.ref, artifact: item.artifact, plan: &plan})
	}
	return rejectAgentArtifactCollisions(kept)
}

// replanAgentArtifact plans a verified contribution again against the tree
// as it is now, with the ownership record as it is now.
func replanAgentArtifact(wsRoot string, item preparedAgentArtifact) (*agentartifacts.Plan, error) {
	state, err := agentartifacts.LoadState(wsRoot, item.ref.Name)
	if err != nil {
		return nil, agentArtifactFailure(item.ref.Name, nil, err)
	}
	plan, err := agentartifacts.BuildPlan(wsRoot, item.artifact, state)
	if err != nil {
		return nil, agentArtifactFailure(item.ref.Name, nil, err)
	}
	return plan, nil
}

// planResolvedAgentArtifact runs the materializer's read-only phases against a
// resolved content tree. Every ownership decision belongs to
// internal/agentartifacts: this function chooses no path, compares no digest,
// and skips no check.
func planResolvedAgentArtifact(wsRoot, name string, resolution AgentArtifactResolution) (*agentartifacts.Artifact, *agentartifacts.Plan, error) {
	artifact, err := agentartifacts.LoadArtifact(resolution.Dir, name, resolution.Entry)
	if err != nil {
		return nil, nil, err
	}
	state, err := agentartifacts.LoadState(wsRoot, name)
	if err != nil {
		return nil, nil, err
	}
	plan, err := agentartifacts.BuildPlan(wsRoot, artifact, state)
	if err != nil {
		return nil, nil, err
	}
	return artifact, plan, nil
}

// retiredAgentArtifacts is the completed removal phase: what was retired and,
// per owner, the edited files released to the user.
type retiredAgentArtifacts struct {
	items    []retiredAgentArtifact
	released map[string][]string
}

// applyAgentArtifactRemovals deletes what each retired owner still manages. It
// runs before the opted-in contributions are planned, so a failure afterwards
// leaves files removed and their records intact: the records still authorize
// the rest, and a rerun re-materializes whatever is still opted into.
func applyAgentArtifactRemovals(wsRoot string, retired []retiredAgentArtifact) (map[string][]string, error) {
	released := make(map[string][]string, len(retired))
	for _, item := range retired {
		if item.plan == nil {
			continue
		}
		paths, err := agentartifacts.ApplyRemoval(wsRoot, item.plan)
		if err != nil {
			return nil, agentArtifactFailure(item.name, nil, err)
		}
		released[item.name] = paths
	}
	return released, nil
}

// commitAgentArtifacts is the only writing half of this file, and the order of
// its phases is the run-wide contract:
//
//  1. REJECT — if ANY prepared contribution collides, or if two of them
//     target the same path (including a file/parent overlap), nothing at all
//     is written. Checking here rather than per contribution is what stops a
//     two-extension workspace from materializing the first and then
//     overwriting or blocking the second.
//  2. APPLY — each plan is applied and its ownership recorded. Apply is
//     itself transactional per contribution (stage, then atomic rename), so
//     the only residue a failure here can leave is a completed EARLIER
//     contribution, which reruns as unchanged.
//  3. RETIRE — the pins and records of retired owners are dropped last: until
//     a record is gone it still authorizes the removals already applied, so
//     an interrupted run converges on a rerun.
func commitAgentArtifacts(wsRoot string, prepared []preparedAgentArtifact, retired retiredAgentArtifacts, out io.Writer) error {
	if err := rejectAgentArtifactCollisions(prepared); err != nil {
		return err
	}
	if err := rejectAgentArtifactTargetOverlaps(prepared); err != nil {
		return err
	}
	reports := make([]*agentartifacts.Report, 0, len(prepared))
	for _, item := range prepared {
		if err := agentartifacts.Apply(wsRoot, item.artifact, item.plan); err != nil {
			return agentArtifactFailure(item.ref.Name, nil, err)
		}
		if err := agentartifacts.WriteState(wsRoot, agentartifacts.StateFor(item.artifact)); err != nil {
			return agentArtifactStateFailure(item.ref.Name, err)
		}
		reports = append(reports, agentartifacts.NewReport(item.plan, true))
	}
	if err := dropRetiredAgentArtifactPins(wsRoot, retired.items); err != nil {
		return err
	}
	for _, item := range retired.items {
		if !item.state {
			continue
		}
		if err := agentartifacts.RemoveState(wsRoot, item.name); err != nil {
			return agentArtifactStateFailure(item.name, err)
		}
	}
	for _, report := range reports {
		writeAgentArtifactReport(out, report)
	}
	for _, item := range retired.items {
		writeRetiredAgentArtifactReport(out, item, retired.released[item.name])
	}
	return nil
}

// retiredAgentArtifact is one owner the workspace recorded but no longer opts
// into, carried to the last point before its files are removed.
type retiredAgentArtifact struct {
	name   string
	plan   *agentartifacts.Plan
	pinned bool
	state  bool
}

// planUndeclaredAgentArtifacts finds every owner this clone recorded as
// installed that the workspace no longer opts into, and plans its removal from
// the record alone. It reads and writes nothing else, so it runs before any
// opted-in contribution is applied and an unreadable record fails the whole
// run while nothing has changed yet.
func planUndeclaredAgentArtifacts(wsRoot string, declared []AgentArtifactRef) ([]retiredAgentArtifact, error) {
	readable, err := agentArtifactDeclarationsReadable(wsRoot)
	if err != nil {
		return nil, err
	}
	if !readable {
		// No workspace config to read. "Declares nothing" would be an inference,
		// and retirement is the one destructive path here.
		return nil, nil
	}
	keep := make(map[string]bool, len(declared))
	for _, ref := range declared {
		keep[ref.Name] = true
	}
	states, err := agentartifacts.ListStates(wsRoot)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*retiredAgentArtifact)
	for _, state := range states {
		if keep[state.Name] {
			continue
		}
		plan, err := agentartifacts.BuildRemovalPlan(wsRoot, state)
		if err != nil {
			return nil, agentArtifactFailure(state.Name, nil, err)
		}
		byName[state.Name] = &retiredAgentArtifact{name: state.Name, plan: plan, state: true}
	}
	if len(byName) > 0 {
		lf, err := lockfile.ReadLockFile(wsRoot)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", lockfile.LockFilename, err)
		}
		if lf != nil {
			for name, item := range byName {
				_, item.pinned = lf.GetAgentArtifact(name)
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	retired := make([]retiredAgentArtifact, 0, len(names))
	for _, name := range names {
		retired = append(retired, *byName[name])
	}
	return retired, nil
}

// agentArtifactDeclarationsReadable reports whether the workspace config was
// actually read. wsproto.Load answers an unparseable config with an EMPTY
// config, which is byte-identical to "this workspace declares nothing" — and
// that is what authorizes retirement. A typo in putnami.workspace.json must not
// delete a managed file, so the file is parsed again here and a failure stops
// the phase instead of emptying it.
func agentArtifactDeclarationsReadable(wsRoot string) (bool, error) {
	path := filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	// RawMessage validates the whole document and keeps the parser's error.
	var probe json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return false, protocolcli.WithNext(
			fmt.Errorf("%s does not parse (%w): nothing was changed. An unreadable config cannot be told apart from one that declares no agent content, and that difference decides whether managed files are removed",
				wsproto.WorkspaceConfigFilename, err),
			"putnami validate-workspace")
	}
	return true, nil
}

func writeRetiredAgentArtifactReport(out io.Writer, item retiredAgentArtifact, released []string) {
	if out == nil {
		return
	}
	removed := 0
	if item.plan != nil {
		removed = len(item.plan.Select(agentartifacts.ActionRemove))
	}
	iox.Fprintf(out, "  ✓ %s is no longer declared: %d managed files removed\n", item.name, removed)
	if len(released) > 0 {
		iox.Fprintf(out, "    kept your edited files, no longer managed: %s\n", strings.Join(released, ", "))
	}
}

// AgentWorkflowPhaseApplies reports whether an adopting command has anything
// to do: a contribution to materialize, or an owner the workspace stopped
// opting into.
func AgentWorkflowPhaseApplies(wsRoot string, cfg *wsproto.Config) (bool, error) {
	refs, err := DeclaredAgentArtifacts(wsRoot, cfg)
	if err != nil {
		return false, err
	}
	if len(refs) > 0 {
		return true, nil
	}
	retired, err := planUndeclaredAgentArtifacts(wsRoot, nil)
	if err != nil {
		return false, err
	}
	return len(retired) > 0, nil
}

// EnsureAgentWorkflows is the implicit, lock-driven agent pass a fresh
// worktree runs before its first command: the agent content of every opted-in
// extension the COMMITTED lock pins, or the package manager already
// installed, is materialized exactly as `putnami install` would. It skips an
// unpinned extension and a package not installed yet (explicit install
// installs both first), never builds a local extension's content (that is the
// authoring workspace's explicit `context generate`), never writes the lock,
// and never removes anything. Once the files match, the pass reads and hashes
// a few dozen files and writes nothing.
func EnsureAgentWorkflows(_ context.Context, wsRoot string, cfg *wsproto.Config) error {
	refs, err := DeclaredAgentArtifacts(wsRoot, cfg)
	if err != nil || len(refs) == 0 {
		return err
	}
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return err
	}
	if err := requireNoUnfinishedAgentContentMigration(wsRoot); err != nil {
		return err
	}
	prepared := make([]preparedAgentArtifact, 0, len(refs))
	for _, ref := range refs {
		resolution, pinned, err := pinnedExtensionAgentContent(wsRoot, cfg, lf, ref)
		if err != nil {
			return err
		}
		if !pinned || agentArtifactAlreadyMaterialized(wsRoot, ref.Name, resolution.Entry) {
			continue
		}
		artifact, plan, err := planResolvedAgentArtifact(wsRoot, ref.Name, resolution)
		if err != nil {
			return agentArtifactFailure(ref.Name, nil, err)
		}
		prepared = append(prepared, preparedAgentArtifact{ref: ref, artifact: artifact, plan: plan})
	}
	if len(prepared) == 0 {
		return nil
	}
	mutates := false
	for _, item := range prepared {
		mutates = mutates || item.plan.Mutates() || item.plan.HasCollisions()
	}
	if !mutates {
		// The files are already the pinned bytes but no record proves it (a
		// clone that commits them). Recording ownership writes no workspace
		// file and is what makes every later command take the warm path.
		for _, item := range prepared {
			if item.plan.HasCollisions() {
				continue
			}
			if err := agentartifacts.WriteState(wsRoot, agentartifacts.StateFor(item.artifact)); err != nil {
				return agentArtifactStateFailure(item.ref.Name, err)
			}
		}
		return nil
	}
	return commitAgentArtifacts(wsRoot, prepared, retiredAgentArtifacts{}, nil)
}

// ReconcileAgentWorkflows is the pass an agent session start runs (ADR 0040).
// It brings the agent content of every opted-in extension to the release the
// COMMITTED lock pins, or the package manager installed, so a worktree that
// moved to another lock stops serving the instructions of the lock it was
// installed with. The content is read from the extension release the worktree
// already installed, so it never needs a download.
//
// It is the ensure pass with two differences, each chosen so that a session
// start is never slowed down or blocked by it:
//
//   - the trigger is the ownership record alone. A record that names exactly
//     the pinned version and digests ends the pass for that contribution: no
//     file is hashed and nothing is written;
//   - every failure is returned for the caller to print as a warning. The
//     contributions that did not fail are still reconciled.
//
// Everything else is the ensure pass unchanged: it never resolves, never
// writes the lock, never builds a local extension's content, never retires an
// undeclared owner, and a file the user edited is a collision that aborts the
// write before it starts and is named, with its reason, in the returned error.
func ReconcileAgentWorkflows(_ context.Context, wsRoot string, cfg *wsproto.Config) error {
	refs, err := DeclaredAgentArtifacts(wsRoot, cfg)
	if err != nil || len(refs) == 0 {
		return err
	}
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return err
	}
	if err := requireNoUnfinishedAgentContentMigration(wsRoot); err != nil {
		return err
	}
	var problems []error
	prepared := make([]preparedAgentArtifact, 0, len(refs))
	for _, ref := range refs {
		resolution, pinned, err := pinnedExtensionAgentContent(wsRoot, cfg, lf, ref)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if !pinned {
			continue
		}
		state, err := agentartifacts.LoadState(wsRoot, ref.Name)
		if err != nil {
			problems = append(problems, agentArtifactFailure(ref.Name, nil, err))
			continue
		}
		if stateNamesPin(state, resolution.Entry) {
			continue
		}
		artifact, plan, err := planResolvedAgentArtifact(wsRoot, ref.Name, resolution)
		if err != nil {
			problems = append(problems, agentArtifactFailure(ref.Name, nil, err))
			continue
		}
		prepared = append(prepared, preparedAgentArtifact{ref: ref, artifact: artifact, plan: plan})
	}
	if len(prepared) > 0 {
		if err := commitAgentArtifacts(wsRoot, prepared, retiredAgentArtifacts{}, nil); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// stateNamesPin reports whether an ownership record names exactly the pinned
// bytes. A missing record names nothing: no record proves this clone installed
// the pin.
func stateNamesPin(state *agentartifacts.State, entry lockfile.AgentArtifactLockEntry) bool {
	return state != nil &&
		state.ArtifactVersion == entry.Version &&
		state.ArchiveDigest == entry.Integrity &&
		state.ManifestHash == entry.ManifestHash
}

// agentArtifactAlreadyMaterialized is the ensure pass's warm path: the local
// record names exactly the pinned bytes, and every file it records is still on
// disk with that digest. Anything else — no record, another version, a missing
// or edited file, an unreadable record — falls through to the full plan, which
// is where every decision about writing is made.
func agentArtifactAlreadyMaterialized(wsRoot, name string, entry lockfile.AgentArtifactLockEntry) bool {
	state, err := agentartifacts.LoadState(wsRoot, name)
	if err != nil || !stateNamesPin(state, entry) {
		return false
	}
	for _, file := range state.Files {
		content, err := os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(file.Path)))
		if err != nil || lockfile.HashBytes(content) != file.SHA256 {
			return false
		}
	}
	return true
}

// rejectAgentArtifactTargetOverlaps prevents two independently owned
// contributions from claiming the same workspace target. It also rejects a
// file target that is an ancestor of another target: applying the first would
// turn the second's parent into a file and leave the run partially
// materialized.
//
// Plans include both new files and previously managed removals. Every entry is
// therefore a target for this run and must have exactly one owner before any
// Apply or WriteState call is allowed.
func rejectAgentArtifactTargetOverlaps(prepared []preparedAgentArtifact) error {
	ownersByPath := make(map[string][]string)
	for _, item := range prepared {
		if item.plan == nil {
			continue
		}
		seen := make(map[string]bool, len(item.plan.Entries))
		for _, entry := range item.plan.Entries {
			if seen[entry.Path] {
				continue
			}
			seen[entry.Path] = true
			ownersByPath[entry.Path] = append(ownersByPath[entry.Path], item.ref.Name)
		}
	}

	paths := make([]string, 0, len(ownersByPath))
	for target := range ownersByPath {
		paths = append(paths, target)
		sort.Strings(ownersByPath[target])
	}
	sort.Strings(paths)

	overlaps := make([]string, 0)
	for _, target := range paths {
		owners := ownersByPath[target]
		if len(owners) > 1 {
			overlaps = append(overlaps, fmt.Sprintf("%s [%s]", target, strings.Join(owners, ", ")))
		}

		// Each slash-delimited ancestor is a possible file target. Looking it
		// up directly keeps this bounded by path depth rather than plan size.
		for separator := strings.IndexByte(target, '/'); separator >= 0; {
			ancestor := target[:separator]
			if ancestorOwners := ownersByPath[ancestor]; hasDistinctAgentArtifactOwner(ancestorOwners, owners) {
				overlaps = append(overlaps, fmt.Sprintf("%s [%s] overlaps %s [%s]",
					ancestor, strings.Join(ancestorOwners, ", "), target, strings.Join(owners, ", ")))
			}
			next := strings.IndexByte(target[separator+1:], '/')
			if next < 0 {
				break
			}
			separator += next + 1
		}
	}
	if len(overlaps) == 0 {
		return nil
	}
	sort.Strings(overlaps)
	return fmt.Errorf("%w. Nothing was changed. Agent content owners have overlapping targets: %s",
		agentartifacts.ErrCollision, strings.Join(overlaps, "; "))
}

func hasDistinctAgentArtifactOwner(left, right []string) bool {
	for _, leftOwner := range left {
		for _, rightOwner := range right {
			if leftOwner != rightOwner {
				return true
			}
		}
	}
	return false
}

// rejectAgentArtifactCollisions fails the whole run when any contribution
// preserved a file it could not prove it owns, naming each owner with its
// conflicting paths and the reason each was preserved.
//
// It prints paths and reasons and NOTHING else: never a file's contents, never
// a registry URL carrying a token, never the store location. A collision report
// is the one message a user reads while their private files are on the table,
// so it says which file blocked the run and stops there.
func rejectAgentArtifactCollisions(prepared []preparedAgentArtifact) error {
	blocked := make([]string, 0, len(prepared))
	for _, item := range prepared {
		if !item.plan.HasCollisions() {
			continue
		}
		report := agentartifacts.NewReport(item.plan, false)
		conflicts := make([]string, 0, len(report.Collided))
		for _, collision := range report.Collided {
			conflicts = append(conflicts, fmt.Sprintf("%s [%s]", collision.Path, collision.Reason))
		}
		blocked = append(blocked, fmt.Sprintf("%s preserved %s", item.ref.Name, strings.Join(conflicts, ", ")))
	}
	if len(blocked) == 0 {
		return nil
	}
	return fmt.Errorf("%w. Nothing was changed. %s. Revert or move each file, then rerun",
		agentartifacts.ErrCollision, strings.Join(blocked, "; "))
}

// dropRetiredAgentArtifactPins removes, in ONE lock update, the lock pins of
// retired owners that still carry one: a pin moves with the record it belongs
// to. A run that retires nothing pinned leaves the committed lock untouched.
func dropRetiredAgentArtifactPins(wsRoot string, retired []retiredAgentArtifact) error {
	dropping := make([]string, 0, len(retired))
	for _, item := range retired {
		if item.pinned {
			dropping = append(dropping, item.name)
		}
	}
	if len(dropping) == 0 {
		return nil
	}
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return fmt.Errorf("read %s: %w", lockfile.LockFilename, err)
	}
	if lf == nil {
		return nil
	}
	changed := false
	for _, name := range dropping {
		if _, ok := lf.GetAgentArtifact(name); !ok {
			continue
		}
		lf.RemoveAgentArtifact(name)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := lockfile.WriteLockFile(wsRoot, lf); err != nil {
		return fmt.Errorf("write %s: %w", lockfile.LockFilename, err)
	}
	return nil
}

// agentArtifactFailure names the owner a failure belongs to and redacts any
// credential the underlying message carried. A resolver or download URL is
// operator input that may embed userinfo or a query token, and this text
// reaches CLI and CI logs.
func agentArtifactFailure(name string, _ *agentartifacts.Report, err error) error {
	return redactedErrorf(err, "%s: %s", name, shared.RedactURLCredentials(err.Error()))
}

// agentArtifactStateFailure reports the one failure in this lifecycle that
// cannot be undone, and says what IS still guaranteed.
//
// Apply publishes with atomic renames, so by the time the ownership record is
// written the previous bytes are gone — there is nothing to roll back TO. What
// remains true is that the workspace is not corrupt and the run is convergent:
// BuildPlan checks byte-identity BEFORE ownership (see plan.go's ownership
// table), so at the same release every published file re-plans as `unchanged`,
// the rerun writes nothing, and the record is simply written again.
func agentArtifactStateFailure(name string, err error) error {
	return protocolcli.WithNext(
		redactedErrorf(err,
			"%s: the agent files were published but recording them failed: %s. "+
				"The files on disk are the correct ones — "+
				"rerun the same command at the same release to finish recording them",
			name, shared.RedactURLCredentials(err.Error())),
		"putnami install")
}

// redactedError carries a message that has already been scrubbed while keeping
// the original error reachable for errors.Is/As.
//
// Error() deliberately does NOT delegate to the wrapped error, which is the
// whole point: the raw text — the one that may quote a credentialed URL — can
// never be printed by formatting this value, however a caller formats it.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }

// Unwrap keeps sentinel matching working, so a redacted collision is still
// errors.Is(err, agentartifacts.ErrCollision).
func (e *redactedError) Unwrap() error { return e.err }

func redactedErrorf(err error, format string, args ...any) error {
	return &redactedError{msg: fmt.Sprintf(format, args...), err: err}
}

// writeAgentArtifactReport renders one applied run as two human lines at most.
// The full machine report belongs to a command that asks for one; a lifecycle
// phase says what it did and gets out of the way.
func writeAgentArtifactReport(out io.Writer, report *agentartifacts.Report) {
	if out == nil || report == nil {
		return
	}
	changed := len(report.Created) + len(report.Updated) + len(report.Removed)
	if changed == 0 {
		iox.Fprintf(out, "  ✓ %s@%s already up to date (%d files)\n", report.Name, report.Version, len(report.Unchanged))
		return
	}
	iox.Fprintf(out, "  ✓ %s@%s (%d created, %d updated, %d removed, %d unchanged)\n",
		report.Name, report.Version, len(report.Created), len(report.Updated), len(report.Removed), len(report.Unchanged))
}

// DescribeAgentWorkflowPlan is `upgrade --dry-run`'s rendering. A dry run
// resolves nothing and downloads nothing — the other upgrade phases make the
// same choice — so it reports the opt-ins and the owners it would retire.
func DescribeAgentWorkflowPlan(wsRoot string, cfg *wsproto.Config, out io.Writer) {
	refs, declErr := DeclaredAgentArtifacts(wsRoot, cfg)
	if declErr != nil {
		iox.Fprintf(out, "  Cannot read the agent content declarations: %v\n", declErr)
		if next := protocolcli.SuggestedNext(declErr); next != "" {
			iox.Fprintf(out, "  Next: %s\n", next)
		}
		return
	}
	retired, retireErr := planUndeclaredAgentArtifacts(wsRoot, refs)
	if retireErr != nil {
		iox.Fprintf(out, "  Cannot read the agent content ownership records: %v\n", retireErr)
		return
	}
	for _, item := range retired {
		removed := 0
		if item.plan != nil {
			removed = len(item.plan.Select(agentartifacts.ActionRemove))
		}
		iox.Fprintf(out, "  Would remove %s, which is no longer declared (%d managed files)\n", item.name, removed)
	}
	if len(refs) == 0 {
		if len(retired) == 0 {
			iox.Fprintln(out, "  No agent content is opted into. Skipping.")
		}
		return
	}
	for _, ref := range refs {
		pin := "pinned by the extension"
		if declared, err := extension.FindDeclaredExtension(wsRoot, cfg, ref.Extension); err == nil && declared.Package {
			pin = "the package the package manager installs"
		}
		iox.Fprintf(out, "  Would materialize the agent content of extension %s at the version the extension resolves to (%s)\n", ref.Extension, pin)
	}
}
