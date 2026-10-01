package agentctx

import (
	"bytes"
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
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// Agent-content migration: `putnami migrate agent-content <extension>`.
//
// A workspace that declared its agent workflows as separate artifacts — the
// legacy entries this CLI no longer installs (agent_content_legacy.go) — moves
// them to the agent content of the extension that supersedes them
// (agentContent.supersedes) in one explicit, preflighted step. This is the one
// command that reads those entries, their lock pins and their ownership
// records, and it only ever moves them to the extension. The step has two
// halves, and the report shows both:
//
//   - MECHANICAL: the superseded `agentArtifacts` entries give way to one
//     `extension:<name>` entry, their lock pins are dropped so the extension's
//     own pin is the only one left, and their ownership records merge into the
//     extension's record. Nothing is resolved: the extension is read at the
//     release the lock already pins (or built from its local source), so no
//     release is ever substituted.
//   - SEMANTIC: the extension's content is planned against the merged
//     ownership history with the ordinary ownership table (ADR 0004). The
//     report lists every file the content adds, changes or no longer ships. A
//     file the user edited that the content still ships blocks the migration;
//     one the content no longer ships is kept and released, as retirement
//     releases it (ADR 0047 §6).
//
// The whole migration is planned before anything is written. Its first write
// is a journal under .putnami/ holding every piece's state before and after,
// plus a copy of every file the migration replaces or removes. The pieces then
// move in a fixed order — files, the extension's ownership record, the lock,
// the workspace declarations, the superseded records — and the journal is
// marked complete last. A rerun resumes from the journal; `--rollback` drives
// every piece back to its recorded before state. Both accept a piece only in
// its recorded before or after state, so neither overwrites a change made
// since.

// Migration modes.
const (
	// AgentContentMigrationCheck plans the migration and writes nothing.
	AgentContentMigrationCheck = "check"
	// AgentContentMigrationApply performs the migration, or resumes one that
	// stopped.
	AgentContentMigrationApply = "apply"
	// AgentContentMigrationRollback restores the state the journal recorded
	// before the migration.
	AgentContentMigrationRollback = "rollback"
)

// Migration outcomes (AgentContentMigrationReport.Outcome).
const (
	// AgentContentMigrationClean: nothing that the extension supersedes is
	// declared, pinned or recorded, so there is nothing to move.
	AgentContentMigrationClean = "clean"
	// AgentContentMigrationPending: check mode found a migration to perform.
	AgentContentMigrationPending = "pending"
	// AgentContentMigrationApplied: apply mode completed the migration.
	AgentContentMigrationApplied = "applied"
	// AgentContentMigrationRolledBack: rollback mode restored the recorded
	// before state.
	AgentContentMigrationRolledBack = "rolled-back"
)

// Content changes (AgentContentMigrationFile.Change).
const (
	// AgentContentFileAdded: the content ships a path nothing managed before.
	AgentContentFileAdded = "added"
	// AgentContentFileChanged: a managed path gets the content's bytes.
	AgentContentFileChanged = "changed"
	// AgentContentFileRemoved: a managed path the content no longer ships.
	AgentContentFileRemoved = "removed"
	// AgentContentFileReleased: a path the content no longer ships and the
	// user edited; it is kept and no longer managed.
	AgentContentFileReleased = "released"
	// AgentContentFileBlocked: a path the content ships that is, or may be,
	// the user's; it blocks the whole migration.
	AgentContentFileBlocked = "blocked"
)

// Ownership sources (AgentContentMigrationArtifact.Ownership).
const (
	// AgentContentOwnershipRecord: this clone's ownership record for the
	// artifact moves to the extension.
	AgentContentOwnershipRecord = "record"
	// AgentContentOwnershipNone: this clone holds no record for the artifact,
	// so no file is proven to be its own; only its declaration and pin move.
	// The content still adopts, by bytes, every file already identical to
	// what it ships.
	AgentContentOwnershipNone = "none"
)

// AgentContentMigrationReport is the machine-readable result of
// `putnami migrate agent-content`. Every collection is sorted and it carries
// no timestamp, so two runs over the same workspace render identical bytes.
type AgentContentMigrationReport struct {
	// Extension is the extension whose agent content receives the artifacts.
	Extension string `json:"extension"`
	// Version is the release of that content: the extension's lock pin, or
	// the content-derived version of a local extension's build.
	Version string `json:"version,omitempty"`
	// Outcome is one of the AgentContentMigration* outcomes.
	Outcome string `json:"outcome"`
	// Resumed reports a run that continued a migration which had stopped.
	Resumed bool `json:"resumed,omitempty"`
	// Superseded are the artifacts that move, in name order.
	Superseded []AgentContentMigrationArtifact `json:"superseded,omitempty"`
	// Declarations is `agentArtifacts` in putnami.workspace.json before and
	// after the run.
	Declarations AgentContentMigrationDeclarations `json:"declarations"`
	// Files are the content changes, in path order.
	Files []AgentContentMigrationFile `json:"files,omitempty"`
	// Unchanged counts the managed paths whose bytes do not change.
	Unchanged int `json:"unchanged"`
}

// AgentContentMigrationArtifact is one superseded artifact.
type AgentContentMigrationArtifact struct {
	// Name is the artifact identity.
	Name string `json:"name"`
	// Declared is its authored `agentArtifacts` entry, empty when the
	// workspace no longer declares it.
	Declared string `json:"declared,omitempty"`
	// Pin is the version putnami.lock.json pins, empty when unpinned.
	Pin string `json:"pin,omitempty"`
	// Ownership is one of the AgentContentOwnership* sources.
	Ownership string `json:"ownership"`
	// Files counts the managed paths whose ownership moves.
	Files int `json:"files"`
}

// AgentContentMigrationDeclarations is the `agentArtifacts` array before and
// after a run.
type AgentContentMigrationDeclarations struct {
	// Before is the array as the workspace file declared it before the run.
	Before []string `json:"before"`
	// After is the array the run leaves in the workspace file.
	After []string `json:"after"`
}

// AgentContentMigrationFile is one content change.
type AgentContentMigrationFile struct {
	// Path is the workspace-relative path.
	Path string `json:"path"`
	// Change is one of the AgentContentFile* kinds.
	Change string `json:"change"`
	// Before is the SHA-256 of the bytes the path held, empty when absent.
	Before string `json:"before,omitempty"`
	// After is the SHA-256 of the bytes the path gets, empty when removed.
	After string `json:"after,omitempty"`
	// Reason names why a blocked path is preserved.
	Reason string `json:"reason,omitempty"`
}

// Journal layout: one directory per extension under .putnami/, holding the
// journal document and a copy of every file the migration replaces or removes.
const (
	agentContentMigrationDirName = "agent-content-migrations"
	agentContentJournalFilename  = "journal.json"
	agentContentJournalBeforeDir = "before"
	agentContentJournalConfig    = "workspace-config.before"
	agentContentJournalVersion   = 1
)

// Journal phases.
const (
	journalApplying    = "applying"
	journalApplied     = "applied"
	journalRollingBack = "rolling-back"
)

// Step names, in the order each drive runs them. A stopped run names the
// step it stopped after.
var (
	// AgentContentMigrationSteps are the apply steps.
	AgentContentMigrationSteps = []string{"journal", "files", "ownership", "lock", "declarations", "records", "complete"}
	// AgentContentRollbackSteps are the rollback steps.
	AgentContentRollbackSteps = []string{"journal", "files", "records", "lock", "declarations", "ownership", "complete"}
)

// AgentContentMigrationInterrupt is the seam tests replace to stop a
// migration or a rollback right after a named step, exactly as a crash or a
// failed write would. Production never interrupts.
var AgentContentMigrationInterrupt = func(step string) error { return nil }

// agentContentJournal is the durable plan of one migration: each piece's
// state before and after, and the phase reached.
type agentContentJournal struct {
	Version      int                               `json:"version"`
	Extension    string                            `json:"extension"`
	Phase        string                            `json:"phase"`
	Content      agentContentIdentity              `json:"content"`
	Declarations AgentContentMigrationDeclarations `json:"declarations"`
	// Config binds the workspace config bytes before and after the
	// migration; the before bytes are kept beside the journal, so a rollback
	// restores the file exactly while it is still what the migration wrote.
	Config     journalConfig     `json:"config"`
	Superseded []journalArtifact `json:"superseded"`
	// Target is the extension's ownership record before the migration; nil
	// when the extension's content was not installed.
	Target *agentartifacts.State `json:"target,omitempty"`
	// After is the extension's ownership record the migration writes.
	After *agentartifacts.State `json:"after"`
	// Files are the paths the migration writes or removes.
	Files []journalFile `json:"files,omitempty"`
	// Released are the edited paths the content no longer ships.
	Released  []string `json:"released,omitempty"`
	Unchanged int      `json:"unchanged"`
}

// journalConfig is the SHA-256 of the workspace config before and after the
// migration.
type journalConfig struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// agentContentIdentity names the exact content release a journal was planned
// against; a resumed run must find the same one.
type agentContentIdentity struct {
	Version       string `json:"version"`
	ArchiveDigest string `json:"archiveDigest"`
	ManifestHash  string `json:"manifestHash"`
}

// journalArtifact is one superseded artifact's before state.
type journalArtifact struct {
	Name     string                           `json:"name"`
	Declared string                           `json:"declared,omitempty"`
	Pin      *lockfile.AgentArtifactLockEntry `json:"pin,omitempty"`
	// Record is the artifact's ownership record before the migration; nil
	// when this clone had none.
	Record    *agentartifacts.State `json:"record,omitempty"`
	Ownership string                `json:"ownership"`
	// History is the ownership proof that moves: the record's files, or the
	// files adopted by bytes.
	History []agentartifacts.File `json:"history,omitempty"`
}

// journalFile is one path the migration writes or removes. An empty digest
// means the path is absent in that state.
type journalFile struct {
	Path   string `json:"path"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// agentContentMigration is one prepared run: the verified content, the plan,
// and the journal that describes it.
type agentContentMigration struct {
	wsRoot   string
	name     string
	artifact *agentartifacts.Artifact
	journal  *agentContentJournal
	resumed  bool
	// clean holds the declarations of a workspace with nothing to migrate.
	clean []string
	// applied is the content plan without the released paths.
	applied  *agentartifacts.Plan
	blocking []agentartifacts.Entry
}

// MigrateAgentContent runs `putnami migrate agent-content <extension>` in one
// of its modes: check (the default) plans and writes nothing, apply performs
// or resumes the migration, and rollback restores the state recorded before
// it.
//
// Exit-code contract, as for `migrate vnext`: a pending migration in check
// mode is an invalid-config failure (exit 2) that carries the report; a clean
// check, a completed migration and a completed rollback succeed.
func MigrateAgentContent(_ context.Context, wsRoot string, cfg *wsproto.Config, name, mode, outputFormat string) error {
	name = strings.TrimSpace(name)
	if !extensionNameFormat.MatchString(name) {
		return protocolcli.Classify(fmt.Errorf(
			"migrate agent-content: %q is not an extension name (@scope/name or name: lowercase letters, digits and hyphens)", name),
			protocolcli.ErrUsage)
	}
	var (
		report AgentContentMigrationReport
		err    error
	)
	switch mode {
	case AgentContentMigrationCheck, "":
		mode = AgentContentMigrationCheck
		report, err = checkAgentContentMigration(wsRoot, cfg, name)
	case AgentContentMigrationApply:
		report, err = applyAgentContentMigration(wsRoot, cfg, name)
	case AgentContentMigrationRollback:
		report, err = rollbackAgentContentMigration(wsRoot, name)
	default:
		return protocolcli.Classify(fmt.Errorf("migrate agent-content: unknown mode %q", mode), protocolcli.ErrUsage)
	}
	return renderAgentContentMigration(outputFormat, mode, report, err)
}

func checkAgentContentMigration(wsRoot string, cfg *wsproto.Config, name string) (AgentContentMigrationReport, error) {
	m, err := prepareAgentContentMigration(wsRoot, cfg, name)
	if err != nil {
		return AgentContentMigrationReport{}, err
	}
	if m.journal == nil {
		return m.cleanReport(), nil
	}
	report := m.report(AgentContentMigrationPending)
	if len(m.blocking) > 0 {
		return report, m.collisionError()
	}
	return report, nil
}

func applyAgentContentMigration(wsRoot string, cfg *wsproto.Config, name string) (AgentContentMigrationReport, error) {
	lock, err := lockAgentContentMigrations(wsRoot)
	if err != nil {
		return AgentContentMigrationReport{}, err
	}
	defer func() { _ = lock.Release() }()
	m, err := prepareAgentContentMigration(wsRoot, cfg, name)
	if err != nil {
		return AgentContentMigrationReport{}, err
	}
	if m.journal == nil {
		return m.cleanReport(), nil
	}
	if len(m.blocking) > 0 {
		return m.report(AgentContentMigrationPending), m.collisionError()
	}
	if err := m.drive(); err != nil {
		return m.report(AgentContentMigrationPending), err
	}
	return m.report(AgentContentMigrationApplied), nil
}

// prepareAgentContentMigration plans a migration without writing to the
// workspace. It returns a migration whose journal is nil when there is
// nothing to move.
func prepareAgentContentMigration(wsRoot string, cfg *wsproto.Config, name string) (*agentContentMigration, error) {
	readable, err := agentArtifactDeclarationsReadable(wsRoot)
	if err != nil {
		return nil, err
	}
	if !readable {
		return nil, fmt.Errorf("no %s in %s: an agent-content migration edits the workspace declarations",
			wsproto.WorkspaceConfigFilename, wsRoot)
	}
	journals, err := listAgentContentJournals(wsRoot)
	if err != nil {
		return nil, err
	}
	var existing *agentContentJournal
	for _, journal := range journals {
		if journal.Extension == name {
			existing = journal
			continue
		}
		if journal.Phase != journalApplied {
			return nil, unfinishedAgentContentMigration(journal)
		}
	}
	if existing != nil && existing.Phase == journalRollingBack {
		return nil, unfinishedAgentContentMigration(existing)
	}

	source, err := extension.LocateAgentContent(wsRoot, cfg, name)
	if err != nil {
		return nil, agentArtifactFailure(name, nil, err)
	}
	resolution, err := resolveAgentContentSource(wsRoot, source)
	if err != nil {
		return nil, agentArtifactFailure(name, nil, err)
	}
	artifact, err := agentartifacts.LoadArtifact(resolution.Dir, name, resolution.Entry)
	if err != nil {
		return nil, agentArtifactFailure(name, nil, err)
	}
	m := &agentContentMigration{wsRoot: wsRoot, name: name, artifact: artifact}

	if existing != nil && existing.Phase == journalApplying {
		if existing.Content != identityOf(artifact) {
			return nil, protocolcli.WithNext(fmt.Errorf(
				"the agent-content migration to %s stopped at release %s, but %s now resolves to %s: nothing was changed. "+
					"A migration finishes against the release it was planned for; restore that release, or roll the migration back",
				name, existing.Content.Version, name, artifact.Version),
				"putnami migrate agent-content "+name+" --rollback")
		}
		m.journal = existing
		m.resumed = true
	} else {
		journal, clean, err := planAgentContentJournal(wsRoot, cfg, source, artifact)
		if err != nil {
			return nil, err
		}
		if journal == nil {
			m.clean = clean
			return m, nil
		}
		if existing != nil {
			return nil, completedAgentContentMigrationKept(existing)
		}
		m.journal = journal
	}
	if err := m.verifyPieces(); err != nil {
		return nil, err
	}
	if err := m.planContent(); err != nil {
		return nil, err
	}
	return m, nil
}

// planAgentContentJournal computes the journal of a fresh migration: which
// artifacts the extension supersedes here, the ownership each one hands over,
// and the declarations after the move. It returns a nil journal, with the
// current declarations, when nothing the extension supersedes is declared,
// pinned or recorded.
func planAgentContentJournal(wsRoot string, cfg *wsproto.Config, source *extension.AgentContentSource, artifact *agentartifacts.Artifact) (*agentContentJournal, []string, error) {
	name := source.Name
	supersedes := make(map[string]bool, len(source.Contribution.Supersedes))
	for _, superseded := range source.Contribution.Supersedes {
		supersedes[superseded] = true
	}
	if len(supersedes) == 0 {
		return nil, nil, fmt.Errorf(
			"extension %s@%s names no agent artifact its content supersedes (agentContent.supersedes), so this release offers no migration: "+
				"opt into its content with extension:%s directly, or install a release of %s that declares what it replaces",
			name, artifact.Version, name, name)
	}

	declarations, err := readWorkspaceDeclarations(wsRoot)
	if err != nil {
		return nil, nil, err
	}
	declaredBy := make(map[string]string)
	optedIn := false
	for _, entry := range declarations {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		if ref, ok, err := parseExtensionAgentContentReference(strings.TrimSpace(entry)); ok {
			if err != nil {
				return nil, nil, err
			}
			if ref.Name == name {
				optedIn = true
			}
			if supersedes[ref.Name] {
				return nil, nil, fmt.Errorf(
					"%s supersedes %s, which this workspace opts into as the agent content of an extension: a migration moves separately declared artifacts only",
					name, ref.Name)
			}
			continue
		}
		legacy, err := parseLegacyAgentArtifact(wsRoot, entry)
		if err != nil {
			var missing *missingLegacyProjectError
			if !errors.As(err, &missing) {
				return nil, nil, err
			}
			// An entry whose project is gone is named by the superseded
			// artifact its path names; one that names none stays as declared.
			legacy = legacyAgentArtifact{Name: nameMissingLegacyProject(missing, sortedNames(supersedes)), Declared: entry}
		}
		if legacy.Name != "" && supersedes[legacy.Name] && declaredBy[legacy.Name] == "" {
			declaredBy[legacy.Name] = entry
		}
	}
	// The loader merges the global putnami config into agentArtifacts; a
	// superseded entry that only it declares is one this migration cannot
	// edit away.
	for _, entry := range cfg.AgentArtifacts {
		if superseded := migrationDeclarationName(wsRoot, entry, supersedes); supersedes[superseded] && declaredBy[superseded] == "" {
			return nil, nil, fmt.Errorf(
				"%s is declared outside %s (in the global putnami config): the migration edits only the workspace declarations; remove it there, then rerun",
				superseded, wsproto.WorkspaceConfigFilename)
		}
	}

	states, err := agentartifacts.ListStates(wsRoot)
	if err != nil {
		return nil, nil, err
	}
	statesByName := make(map[string]*agentartifacts.State, len(states))
	for _, state := range states {
		statesByName[state.Name] = state
	}
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", lockfile.LockFilename, err)
	}

	names := make([]string, 0, len(supersedes))
	for superseded := range supersedes {
		_, pinned := lockPin(lf, superseded)
		if declaredBy[superseded] != "" || statesByName[superseded] != nil || pinned {
			names = append(names, superseded)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, declarations, nil
	}

	journal := &agentContentJournal{
		Version:   agentContentJournalVersion,
		Extension: name,
		Phase:     journalApplying,
		Content:   identityOf(artifact),
		Target:    statesByName[name],
		After:     agentartifacts.StateFor(artifact),
	}
	for _, superseded := range names {
		item := journalArtifact{Name: superseded, Declared: declaredBy[superseded], Record: statesByName[superseded]}
		if pin, ok := lockPin(lf, superseded); ok {
			item.Pin = &pin
		}
		item.Ownership = AgentContentOwnershipNone
		if item.Record != nil {
			item.Ownership = AgentContentOwnershipRecord
			item.History = item.Record.Files
		}
		journal.Superseded = append(journal.Superseded, item)
	}
	journal.Declarations = AgentContentMigrationDeclarations{
		Before: declarations,
		After:  migratedDeclarations(wsRoot, declarations, supersedes, name, optedIn),
	}
	config, err := os.ReadFile(filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename))
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	journal.Config = journalConfig{Before: lockfile.HashBytes(config), After: lockfile.HashBytes(config)}
	if !equalStrings(journal.Declarations.Before, journal.Declarations.After) {
		rendered, err := renderWorkspaceDeclarations(config, journal.Declarations.After)
		if err != nil {
			return nil, nil, err
		}
		journal.Config.After = lockfile.HashBytes(rendered)
	}
	return journal, nil, nil
}

// migratedDeclarations drops every superseded entry and puts the extension's
// opt-in where the first of them was, or last when none was declared here.
func migratedDeclarations(wsRoot string, declarations []string, supersedes map[string]bool, name string, optedIn bool) []string {
	optIn := extensionAgentContentPrefix + name
	after := make([]string, 0, len(declarations)+1)
	placed := optedIn
	for _, entry := range declarations {
		if supersedes[migrationDeclarationName(wsRoot, entry, supersedes)] {
			if !placed {
				after = append(after, optIn)
				placed = true
			}
			continue
		}
		after = append(after, entry)
	}
	if !placed {
		after = append(after, optIn)
	}
	return after
}

// migrationDeclarationName names what one `agentArtifacts` entry declares, as
// AgentArtifactDeclarationName does, for a migration to an extension that
// supersedes the names in supersedes: an in-tree entry whose project is gone
// is named by the superseded artifact its path names (nameMissingLegacyProject).
func migrationDeclarationName(wsRoot, entry string, supersedes map[string]bool) string {
	if name := AgentArtifactDeclarationName(wsRoot, entry); name != "" {
		return name
	}
	var missing *missingLegacyProjectError
	if _, err := parseLegacyAgentArtifact(wsRoot, entry); errors.As(err, &missing) {
		return nameMissingLegacyProject(missing, sortedNames(supersedes))
	}
	return ""
}

// planContent builds the semantic half: the extension's content planned
// against the ownership history the superseded artifacts hand over.
func (m *agentContentMigration) planContent() error {
	transferred, err := mergeOwnershipHistories(m.journal)
	if err != nil {
		return err
	}
	plan, err := agentartifacts.BuildPlan(m.wsRoot, m.artifact, &agentartifacts.State{
		Version: agentartifacts.StateVersion,
		Name:    m.name,
		Files:   transferred,
	})
	if err != nil {
		return agentArtifactFailure(m.name, nil, err)
	}
	if err := m.rejectRemainingOwners(plan); err != nil {
		return err
	}

	ships := m.artifact.Digests()
	recorded := make(map[string]string, len(transferred))
	for _, file := range transferred {
		recorded[file.Path] = file.SHA256
	}
	applied := &agentartifacts.Plan{
		Name:          plan.Name,
		Version:       plan.Version,
		ArchiveDigest: plan.ArchiveDigest,
		ManifestHash:  plan.ManifestHash,
	}
	var (
		files     []journalFile
		released  []string
		unchanged int
	)
	m.blocking = nil
	for _, entry := range plan.Entries {
		if entry.Action == agentartifacts.ActionCollide {
			if _, shipped := ships[entry.Path]; shipped {
				m.blocking = append(m.blocking, entry)
			} else {
				released = append(released, entry.Path)
			}
			continue
		}
		applied.Entries = append(applied.Entries, entry)
		switch entry.Action {
		case agentartifacts.ActionCreate:
			files = append(files, journalFile{Path: entry.Path, After: entry.SHA256})
		case agentartifacts.ActionUpdate:
			files = append(files, journalFile{Path: entry.Path, Before: recorded[entry.Path], After: entry.SHA256})
		case agentartifacts.ActionRemove:
			files = append(files, journalFile{Path: entry.Path, Before: entry.SHA256})
		case agentartifacts.ActionUnchanged:
			unchanged++
		}
	}
	m.applied = applied
	if !m.resumed {
		// A resumed run keeps the change set it was planned with: part of it
		// is already on disk and now plans as unchanged.
		m.journal.Files = files
		m.journal.Released = released
		m.journal.Unchanged = unchanged
	}
	return nil
}

// mergeOwnershipHistories merges the ownership each superseded artifact hands
// over with the extension's own record. One path recorded by two owners with
// the same digest is one fact and merges; with two different digests it is a
// duplicate ownership nobody can resolve by rule, so the migration refuses
// rather than pick the owner whose bytes it would then overwrite.
func mergeOwnershipHistories(journal *agentContentJournal) ([]agentartifacts.File, error) {
	type claim struct{ owner, digest string }
	claims := make(map[string][]claim)
	add := func(owner string, files []agentartifacts.File) {
		for _, file := range files {
			claims[file.Path] = append(claims[file.Path], claim{owner: owner, digest: file.SHA256})
		}
	}
	for _, item := range journal.Superseded {
		add(item.Name, item.History)
	}
	if journal.Target != nil {
		add(journal.Extension, journal.Target.Files)
	}
	paths := make([]string, 0, len(claims))
	for path := range claims {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	merged := make([]agentartifacts.File, 0, len(paths))
	var conflicts []string
	for _, path := range paths {
		owners := claims[path]
		agreed := true
		for _, other := range owners[1:] {
			if other.digest != owners[0].digest {
				agreed = false
			}
		}
		if agreed {
			merged = append(merged, agentartifacts.File{Path: path, SHA256: owners[0].digest})
			continue
		}
		described := make([]string, 0, len(owners))
		for _, owner := range owners {
			described = append(described, owner.owner+" "+owner.digest[:12])
		}
		sort.Strings(described)
		conflicts = append(conflicts, fmt.Sprintf("%s [%s]", path, strings.Join(described, ", ")))
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf(
			"duplicate ownership: %s. Nothing was changed. Each path has one owner; remove the ownership record under .putnami/%s/ that no longer describes the file on disk, then rerun",
			strings.Join(conflicts, "; "), agentartifacts.StateDirName)
	}
	return merged, nil
}

// rejectRemainingOwners refuses a migration whose content would share a path
// with an artifact that keeps its own record: after the move both records
// would claim it.
func (m *agentContentMigration) rejectRemainingOwners(plan *agentartifacts.Plan) error {
	states, err := agentartifacts.ListStates(m.wsRoot)
	if err != nil {
		return err
	}
	moving := map[string]bool{m.name: true}
	for _, item := range m.journal.Superseded {
		moving[item.Name] = true
	}
	prepared := []preparedAgentArtifact{{ref: AgentArtifactRef{Name: m.name}, plan: plan}}
	for _, state := range states {
		if moving[state.Name] {
			continue
		}
		entries := make([]agentartifacts.Entry, 0, len(state.Files))
		for _, file := range state.Files {
			entries = append(entries, agentartifacts.Entry{Path: file.Path, Action: agentartifacts.ActionUnchanged, SHA256: file.SHA256})
		}
		prepared = append(prepared, preparedAgentArtifact{ref: AgentArtifactRef{Name: state.Name}, plan: &agentartifacts.Plan{Name: state.Name, Entries: entries}})
	}
	if err := rejectAgentArtifactTargetOverlaps(prepared); err != nil {
		return fmt.Errorf("duplicate ownership after the migration: %w", err)
	}
	return nil
}

// verifyPieces accepts every durable piece the journal names only in its
// recorded before or after state. Anything else changed since the journal
// was planned, and moving it either way would overwrite that change.
func (m *agentContentMigration) verifyPieces() error {
	journal := m.journal
	var drift []string
	current, err := agentartifacts.LoadState(m.wsRoot, m.name)
	if err != nil {
		return agentArtifactFailure(m.name, nil, err)
	}
	if !sameState(current, journal.Target) && !sameState(current, journal.After) {
		drift = append(drift, "the ownership record of "+m.name)
	}
	lf, err := lockfile.ReadLockFile(m.wsRoot)
	if err != nil {
		return fmt.Errorf("read %s: %w", lockfile.LockFilename, err)
	}
	for _, item := range journal.Superseded {
		record, err := agentartifacts.LoadState(m.wsRoot, item.Name)
		if err != nil {
			return agentArtifactFailure(item.Name, nil, err)
		}
		if record != nil && !sameState(record, item.Record) {
			drift = append(drift, "the ownership record of "+item.Name)
		}
		pin, pinned := lockPin(lf, item.Name)
		switch {
		case item.Pin != nil && lf == nil:
			drift = append(drift, lockfile.LockFilename)
		case pinned && (item.Pin == nil || pin != *item.Pin):
			drift = append(drift, "the lock pin of "+item.Name)
		}
	}
	declarations, err := readWorkspaceDeclarations(m.wsRoot)
	if err != nil {
		return err
	}
	if !equalStrings(declarations, journal.Declarations.Before) && !equalStrings(declarations, journal.Declarations.After) {
		drift = append(drift, "agentArtifacts in "+wsproto.WorkspaceConfigFilename)
	}
	if len(drift) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s changed since the agent-content migration to %s was planned: nothing was changed. "+
			"A migration moves each piece only from the state it recorded, and never over a change made since; restore that state, "+
			"or remove .putnami/%s/%s to give the migration up",
		strings.Join(dedupe(drift), ", "), m.name, agentContentMigrationDirName, layout.EncodeName(m.name))
}

// drive performs the migration: the journal first, then every piece in a
// fixed order, the journal's completion last. Each step is idempotent, so a
// rerun after any stop repeats nothing it already did.
func (m *agentContentMigration) drive() error {
	journal := m.journal
	steps := []func() error{
		func() error {
			if m.resumed {
				return nil
			}
			return startAgentContentJournal(m.wsRoot, journal)
		},
		func() error { return agentartifacts.Apply(m.wsRoot, m.artifact, m.applied) },
		func() error { return agentartifacts.WriteState(m.wsRoot, journal.After) },
		m.dropSupersededPins,
		func() error { return writeWorkspaceDeclarations(m.wsRoot, journal.Declarations.After) },
		func() error {
			for _, item := range journal.Superseded {
				if err := agentartifacts.RemoveState(m.wsRoot, item.Name); err != nil {
					return err
				}
			}
			return nil
		},
		func() error {
			journal.Phase = journalApplied
			return saveAgentContentJournal(m.wsRoot, journal)
		},
	}
	return runAgentContentSteps(AgentContentMigrationSteps, steps, func(step string, err error) error {
		if step == "journal" && !m.resumed {
			if started, _ := loadAgentContentJournal(m.wsRoot, m.name); started == nil {
				return fmt.Errorf("start the agent-content migration to %s: %w. Nothing was changed", m.name, err)
			}
		}
		return protocolcli.WithNext(fmt.Errorf(
			"the agent-content migration to %s stopped during %s: %w. Its journal records every piece's state; rerun to finish it, or roll it back",
			m.name, step, err), "putnami migrate agent-content "+m.name+" --apply")
	})
}

// runAgentContentSteps runs the steps in order and stops at the first
// failure. The interrupt seam fires between steps, never after the last one:
// a run that completed has nothing left to stop.
func runAgentContentSteps(names []string, steps []func() error, stopped func(step string, err error) error) error {
	for index, step := range steps {
		if err := step(); err != nil {
			return stopped(names[index], err)
		}
		if index == len(steps)-1 {
			break
		}
		if err := AgentContentMigrationInterrupt(names[index]); err != nil {
			return stopped(names[index], err)
		}
	}
	return nil
}

// dropSupersededPins removes the superseded pins in one lock write; the
// extension's own pin is untouched and becomes the content's only pin.
func (m *agentContentMigration) dropSupersededPins() error {
	lf, err := lockfile.ReadLockFile(m.wsRoot)
	if err != nil || lf == nil {
		return err
	}
	changed := false
	for _, item := range m.journal.Superseded {
		if _, ok := lf.GetAgentArtifact(item.Name); ok {
			lf.RemoveAgentArtifact(item.Name)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return lockfile.WriteLockFile(m.wsRoot, lf)
}

// rollbackAgentContentMigration restores what the journal recorded before the
// migration: files first, then the superseded records, pins and declarations,
// then the extension's record, and the journal is removed last.
func rollbackAgentContentMigration(wsRoot, name string) (AgentContentMigrationReport, error) {
	lock, err := lockAgentContentMigrations(wsRoot)
	if err != nil {
		return AgentContentMigrationReport{}, err
	}
	defer func() { _ = lock.Release() }()
	journal, err := loadAgentContentJournal(wsRoot, name)
	if err != nil {
		return AgentContentMigrationReport{}, err
	}
	if journal == nil {
		return AgentContentMigrationReport{}, protocolcli.NotFoundf(
			"no agent-content migration to %s is recorded in this clone: there is nothing to roll back", name)
	}
	m := &agentContentMigration{wsRoot: wsRoot, name: name, journal: journal}
	if err := m.verifyPieces(); err != nil {
		return AgentContentMigrationReport{}, err
	}
	before := journal.beforeArtifact(wsRoot)
	var after []agentartifacts.File
	for _, file := range journal.Files {
		if file.After != "" {
			after = append(after, agentartifacts.File{Path: file.Path, SHA256: file.After})
		}
	}
	plan, err := agentartifacts.BuildPlan(wsRoot, before, &agentartifacts.State{Version: agentartifacts.StateVersion, Name: name, Files: after})
	if err != nil {
		return AgentContentMigrationReport{}, agentArtifactFailure(name, nil, err)
	}
	report := rollbackReport(journal, plan)
	if plan.HasCollisions() {
		var conflicts []string
		for _, entry := range plan.Collisions() {
			conflicts = append(conflicts, fmt.Sprintf("%s [%s]", entry.Path, entry.Reason))
		}
		return report, fmt.Errorf(
			"%w. Nothing was changed. A rollback restores only what the migration to %s wrote, and these files changed since: %s. Revert or move each file, then rerun",
			agentartifacts.ErrCollision, name, strings.Join(conflicts, ", "))
	}
	// Every copied byte is verified before the first write, so a missing or
	// altered copy cannot stop the restore halfway.
	for _, file := range before.Files {
		if _, err := before.Read(file.Path); err != nil {
			return report, fmt.Errorf("the journal's copy of %s cannot restore it: %w. Nothing was changed", file.Path, err)
		}
	}
	if _, err := journalConfigCopy(wsRoot, journal); err != nil {
		return report, fmt.Errorf("the journal's copy of %s cannot restore it: %w. Nothing was changed", wsproto.WorkspaceConfigFilename, err)
	}

	steps := []func() error{
		func() error {
			journal.Phase = journalRollingBack
			return saveAgentContentJournal(wsRoot, journal)
		},
		func() error { return agentartifacts.Apply(wsRoot, before, plan) },
		func() error {
			for _, item := range journal.Superseded {
				if item.Record == nil {
					continue
				}
				if err := agentartifacts.WriteState(wsRoot, item.Record); err != nil {
					return err
				}
			}
			return nil
		},
		func() error { return restoreSupersededPins(wsRoot, journal) },
		func() error { return restoreWorkspaceDeclarations(wsRoot, journal) },
		func() error {
			if journal.Target != nil {
				return agentartifacts.WriteState(wsRoot, journal.Target)
			}
			return agentartifacts.RemoveState(wsRoot, name)
		},
		func() error { return removeAgentContentJournal(wsRoot, name) },
	}
	err = runAgentContentSteps(AgentContentRollbackSteps, steps, func(step string, err error) error {
		return protocolcli.WithNext(fmt.Errorf(
			"the rollback of the agent-content migration to %s stopped during %s: %w. Its journal still records every piece's state; rerun the rollback to finish it",
			name, step, err), "putnami migrate agent-content "+name+" --rollback")
	})
	if err != nil {
		return report, err
	}
	report.Outcome = AgentContentMigrationRolledBack
	return report, nil
}

// restoreWorkspaceDeclarations puts the workspace config back. While the file
// is still exactly what the migration wrote, the original bytes are restored;
// when it changed since in some other member, only `agentArtifacts` is put
// back, so that change survives.
func restoreWorkspaceDeclarations(wsRoot string, journal *agentContentJournal) error {
	path := filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename)
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	switch lockfile.HashBytes(current) {
	case journal.Config.Before:
		return nil
	case journal.Config.After:
		original, err := journalConfigCopy(wsRoot, journal)
		if err != nil {
			return err
		}
		return shared.AtomicWriteFile(path, original)
	}
	return writeWorkspaceDeclarations(wsRoot, journal.Declarations.Before)
}

// journalConfigCopy returns the kept workspace config bytes once they hash to
// the digest the journal recorded.
func journalConfigCopy(wsRoot string, journal *agentContentJournal) ([]byte, error) {
	path := filepath.Join(agentContentJournalDir(wsRoot, journal.Extension), agentContentJournalConfig)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if got := lockfile.HashBytes(data); got != journal.Config.Before {
		return nil, fmt.Errorf("%s hashes to %s, but the journal recorded %s", path, got, journal.Config.Before)
	}
	return data, nil
}

func restoreSupersededPins(wsRoot string, journal *agentContentJournal) error {
	var pins []journalArtifact
	for _, item := range journal.Superseded {
		if item.Pin != nil {
			pins = append(pins, item)
		}
	}
	if len(pins) == 0 {
		return nil
	}
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return err
	}
	if lf == nil {
		return fmt.Errorf("%s is missing, so the superseded pins cannot be restored", lockfile.LockFilename)
	}
	changed := false
	for _, item := range pins {
		if current, ok := lf.GetAgentArtifact(item.Name); ok && current == *item.Pin {
			continue
		}
		lf.SetAgentArtifact(item.Name, *item.Pin)
		changed = true
	}
	if !changed {
		return nil
	}
	return lockfile.WriteLockFile(wsRoot, lf)
}

// beforeArtifact is the journal's copy of the replaced and removed files,
// shaped as an artifact so the materializer restores it with its own
// verified, transactional apply.
func (j *agentContentJournal) beforeArtifact(wsRoot string) *agentartifacts.Artifact {
	dir := filepath.Join(agentContentJournalDir(wsRoot, j.Extension), agentContentJournalBeforeDir)
	var files []agentartifacts.File
	for _, file := range j.Files {
		if file.Before != "" {
			files = append(files, agentartifacts.File{Path: file.Path, SHA256: file.Before})
		}
	}
	return &agentartifacts.Artifact{
		Name:          j.Extension,
		Version:       j.Content.Version,
		ArchiveDigest: j.Content.ArchiveDigest,
		ManifestHash:  j.Content.ManifestHash,
		Dir:           dir,
		Files:         files,
	}
}

func rollbackReport(journal *agentContentJournal, plan *agentartifacts.Plan) AgentContentMigrationReport {
	report := AgentContentMigrationReport{
		Extension:    journal.Extension,
		Version:      journal.Content.Version,
		Outcome:      AgentContentMigrationPending,
		Superseded:   supersededReport(journal),
		Declarations: AgentContentMigrationDeclarations{Before: journal.Declarations.After, After: journal.Declarations.Before},
	}
	for _, entry := range plan.Entries {
		switch entry.Action {
		case agentartifacts.ActionCreate:
			report.Files = append(report.Files, AgentContentMigrationFile{Path: entry.Path, Change: AgentContentFileAdded, After: entry.SHA256})
		case agentartifacts.ActionUpdate:
			report.Files = append(report.Files, AgentContentMigrationFile{Path: entry.Path, Change: AgentContentFileChanged, After: entry.SHA256})
		case agentartifacts.ActionRemove:
			report.Files = append(report.Files, AgentContentMigrationFile{Path: entry.Path, Change: AgentContentFileRemoved, Before: entry.SHA256})
		case agentartifacts.ActionUnchanged:
			report.Unchanged++
		case agentartifacts.ActionCollide:
			report.Files = append(report.Files, AgentContentMigrationFile{Path: entry.Path, Change: AgentContentFileBlocked, Reason: entry.Reason})
		}
	}
	return report
}

func (m *agentContentMigration) cleanReport() AgentContentMigrationReport {
	declarations := m.clean
	if declarations == nil {
		declarations = []string{}
	}
	return AgentContentMigrationReport{
		Extension:    m.name,
		Version:      m.artifact.Version,
		Outcome:      AgentContentMigrationClean,
		Declarations: AgentContentMigrationDeclarations{Before: declarations, After: declarations},
	}
}

func (m *agentContentMigration) report(outcome string) AgentContentMigrationReport {
	journal := m.journal
	report := AgentContentMigrationReport{
		Extension:    m.name,
		Version:      journal.Content.Version,
		Outcome:      outcome,
		Resumed:      m.resumed,
		Superseded:   supersededReport(journal),
		Declarations: journal.Declarations,
		Unchanged:    journal.Unchanged,
	}
	for _, file := range journal.Files {
		change := AgentContentFileChanged
		switch {
		case file.Before == "":
			change = AgentContentFileAdded
		case file.After == "":
			change = AgentContentFileRemoved
		}
		report.Files = append(report.Files, AgentContentMigrationFile{Path: file.Path, Change: change, Before: file.Before, After: file.After})
	}
	for _, path := range journal.Released {
		report.Files = append(report.Files, AgentContentMigrationFile{Path: path, Change: AgentContentFileReleased})
	}
	for _, entry := range m.blocking {
		report.Files = append(report.Files, AgentContentMigrationFile{Path: entry.Path, Change: AgentContentFileBlocked, Reason: entry.Reason})
	}
	sort.SliceStable(report.Files, func(i, j int) bool { return report.Files[i].Path < report.Files[j].Path })
	return report
}

func supersededReport(journal *agentContentJournal) []AgentContentMigrationArtifact {
	out := make([]AgentContentMigrationArtifact, 0, len(journal.Superseded))
	for _, item := range journal.Superseded {
		entry := AgentContentMigrationArtifact{Name: item.Name, Declared: item.Declared, Ownership: item.Ownership, Files: len(item.History)}
		if item.Pin != nil {
			entry.Pin = item.Pin.Version
		}
		out = append(out, entry)
	}
	return out
}

// collisionError names every path that blocks the migration and why, and
// nothing else: never a file's contents.
func (m *agentContentMigration) collisionError() error {
	conflicts := make([]string, 0, len(m.blocking))
	for _, entry := range m.blocking {
		conflicts = append(conflicts, fmt.Sprintf("%s [%s]", entry.Path, entry.Reason))
	}
	return fmt.Errorf("%w. Nothing was changed. The agent content of %s preserved %s. Revert or move each file, then rerun",
		agentartifacts.ErrCollision, m.name, strings.Join(conflicts, ", "))
}

// requireNoUnfinishedAgentContentMigration stops every ordinary agent-workflow
// pass while a migration or its rollback is partway: between two steps the
// files, records, pins and declarations describe neither side, and only the
// migration command can finish or undo that.
func requireNoUnfinishedAgentContentMigration(wsRoot string) error {
	journals, err := listAgentContentJournals(wsRoot)
	if err != nil {
		return err
	}
	for _, journal := range journals {
		if journal.Phase != journalApplied {
			return unfinishedAgentContentMigration(journal)
		}
	}
	return nil
}

// completedAgentContentMigrationKept refuses a new migration to an extension
// while a completed one keeps its rollback point: the new journal would
// replace that one, and with it the only copies of what the completed
// migration replaced. Nothing is written.
func completedAgentContentMigrationKept(journal *agentContentJournal) error {
	dir := filepath.ToSlash(filepath.Join(".putnami", agentContentMigrationDirName, layout.EncodeName(journal.Extension)))
	return protocolcli.WithNext(fmt.Errorf(
		"the completed agent-content migration to %s keeps its rollback point in %s, and a new migration to %s would replace it: nothing was changed. "+
			"Roll the completed migration back first, so the next --apply moves everything in one step; or remove %s to give up that rollback, then rerun",
		journal.Extension, dir, journal.Extension, dir), "putnami migrate agent-content "+journal.Extension+" --rollback")
}

func unfinishedAgentContentMigration(journal *agentContentJournal) error {
	if journal.Phase == journalRollingBack {
		return protocolcli.WithNext(fmt.Errorf(
			"the rollback of the agent-content migration to %s did not finish: nothing was changed. Agent workflows stay as they are until it does",
			journal.Extension), "putnami migrate agent-content "+journal.Extension+" --rollback")
	}
	return protocolcli.WithNext(fmt.Errorf(
		"the agent-content migration to %s did not finish: nothing was changed. Agent workflows stay as they are until it is finished (--apply) or rolled back (--rollback)",
		journal.Extension), "putnami migrate agent-content "+journal.Extension+" --apply")
}

// --- workspace declarations ---

// readWorkspaceDeclarations reads `agentArtifacts` exactly as the workspace
// file declares it, without the global config the loader merges in: this is
// the array a migration rewrites.
func readWorkspaceDeclarations(wsRoot string) ([]string, error) {
	path := filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename)
	raw, err := jsonutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	value, ok := raw.Get("agentArtifacts")
	if !ok || value == nil {
		return []string{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: agentArtifacts must be an array of strings", wsproto.WorkspaceConfigFilename)
	}
	declarations := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s: agentArtifacts must be an array of strings", wsproto.WorkspaceConfigFilename)
		}
		declarations = append(declarations, text)
	}
	return declarations, nil
}

// writeWorkspaceDeclarations replaces `agentArtifacts` and nothing else,
// keeping every other member in its order, and publishes the file
// atomically. It writes nothing when the array already holds the values; an
// empty array removes the member.
func writeWorkspaceDeclarations(wsRoot string, declarations []string) error {
	path := filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename)
	current, err := readWorkspaceDeclarations(wsRoot)
	if err != nil {
		return err
	}
	if equalStrings(current, declarations) {
		return nil
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	data, err := renderWorkspaceDeclarations(original, declarations)
	if err != nil {
		return err
	}
	return shared.AtomicWriteFile(path, data)
}

// renderWorkspaceDeclarations returns the workspace config with
// `agentArtifacts` replaced, every other member kept in its order.
func renderWorkspaceDeclarations(config []byte, declarations []string) ([]byte, error) {
	raw := jsonutil.New()
	if err := json.Unmarshal(config, raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	if len(declarations) == 0 {
		raw.Delete("agentArtifacts")
	} else {
		raw.Set("agentArtifacts", declarations)
	}
	return raw.Bytes()
}

// --- rendering ---

func renderAgentContentMigration(outputFormat, mode string, report AgentContentMigrationReport, runErr error) error {
	command := "migrate agent-content"
	if runErr != nil {
		if report.Extension != "" {
			if !protocolcli.OutputMode(outputFormat).IsStructured() {
				printAgentContentMigration(iox.Stdout(), report)
			}
			return shared.WithResultData(runErr, report)
		}
		return runErr
	}
	var err error
	if mode == AgentContentMigrationCheck && report.Outcome == AgentContentMigrationPending {
		err = shared.WithResultData(protocolcli.WithNext(protocolcli.Classify(
			fmt.Errorf("the agent artifacts %s supersedes need migration: %d superseded, %d file change(s)",
				report.Extension, len(report.Superseded), len(report.Files)),
			protocolcli.ErrInvalidConfig), "putnami migrate agent-content "+report.Extension+" --apply"), report)
	}
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		if err == nil {
			_, werr := protocolcli.WriteResultV2(iox.Stdout(), protocolcli.OutputJSONL, protocolcli.NewResultV2(command, report, nil))
			return werr
		}
		return err
	}
	printAgentContentMigration(iox.Stdout(), report)
	return err
}

func printAgentContentMigration(w io.Writer, report AgentContentMigrationReport) {
	iox.Fprintf(w, "\n  Agent-content migration to %s@%s\n\n", report.Extension, report.Version)
	if report.Outcome == AgentContentMigrationClean {
		iox.Fprintf(w, "    nothing to migrate: this workspace declares, pins and records none of the agent artifacts %s supersedes\n\n", report.Extension)
		return
	}
	if report.Resumed {
		iox.Fprintln(w, "    resuming a migration that stopped; the changes below are the ones it was planned with")
	}
	iox.Fprintln(w, "  Moves (mechanical: declarations, pins and ownership):")
	for _, item := range report.Superseded {
		declared, pin := item.Declared, item.Pin
		if declared == "" {
			declared = "not declared"
		}
		if pin == "" {
			pin = "not pinned"
		}
		iox.Fprintf(w, "    %s: %s; %s; ownership %s (%d files)\n", item.Name, declared, pin, item.Ownership, item.Files)
	}
	iox.Fprintf(w, "    agentArtifacts: %s -> %s\n", quotedList(report.Declarations.Before), quotedList(report.Declarations.After))
	iox.Fprintf(w, "    the only pin of this content is the extension's own lock entry (%s)\n", report.Extension)
	iox.Fprintln(w, "\n  Content (semantic: what changes on disk):")
	for _, file := range report.Files {
		line := fmt.Sprintf("    %-9s %s", file.Change, file.Path)
		switch file.Change {
		case AgentContentFileReleased:
			line += " (edited; kept, no longer managed)"
		case AgentContentFileBlocked:
			line += " [" + file.Reason + "]"
		}
		iox.Fprintln(w, line)
	}
	iox.Fprintf(w, "    %d unchanged\n", report.Unchanged)
	switch report.Outcome {
	case AgentContentMigrationPending:
		iox.Fprintf(w, "\n  Run `putnami migrate agent-content %s --apply` to migrate.\n", report.Extension)
	case AgentContentMigrationApplied:
		iox.Fprintf(w, "\n  Migrated. Commit %s and %s; `putnami migrate agent-content %s --rollback` restores the previous state in this clone.\n",
			wsproto.WorkspaceConfigFilename, lockfile.LockFilename, report.Extension)
	case AgentContentMigrationRolledBack:
		iox.Fprintln(w, "\n  Rolled back: declarations, pins, ownership records and files are as they were before the migration.")
	}
	iox.Fprintln(w)
}

func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// --- small helpers ---

func identityOf(artifact *agentartifacts.Artifact) agentContentIdentity {
	return agentContentIdentity{Version: artifact.Version, ArchiveDigest: artifact.ArchiveDigest, ManifestHash: artifact.ManifestHash}
}

func lockPin(lf *lockfile.LockFile, name string) (lockfile.AgentArtifactLockEntry, bool) {
	if lf == nil {
		return lockfile.AgentArtifactLockEntry{}, false
	}
	return lf.GetAgentArtifact(name)
}

// sameState compares two ownership records by their canonical bytes; two
// absent records are the same.
func sameState(a, b *agentartifacts.State) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	left, err := agentartifacts.MarshalState(a)
	if err != nil {
		return false
	}
	right, err := agentartifacts.MarshalState(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
