package agentctx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// Storage of the agent-content migration journal: one directory per extension
// under .putnami/agent-content-migrations/, holding the journal document, a
// copy of the workspace config as it was, and a copy of every file the
// migration replaces or removes. The journal authorizes writes and removals in
// the workspace, so it is strictly decoded, validated against the protocol's
// path rule, and never read or written through a symlink.
// AgentContentMigrationLockFilename is the workspace's migration lock under
// .putnami/. It outlives every run on purpose: removing a lock file another
// process may already have opened would let two runs hold "the" lock at once.
const AgentContentMigrationLockFilename = "agent-content-migrations.lock"

// lockAgentContentMigrations takes the workspace's exclusive migration lock
// without waiting, so two runs that write never interleave their journal,
// file and record writes. A run that finds it held changes nothing.
func lockAgentContentMigrations(wsRoot string) (*flock.Lock, error) {
	dir := filepath.Join(wsRoot, ".putnami")
	if err := checkJournalDirectories(wsRoot, dir, true); err != nil {
		return nil, err
	}
	lock, err := flock.Acquire(filepath.Join(dir, AgentContentMigrationLockFilename), true, true)
	if errors.Is(err, flock.ErrBusy) {
		return nil, fmt.Errorf("another agent-content migration is running in this workspace: nothing was changed; rerun when it finishes")
	}
	if err != nil {
		return nil, fmt.Errorf("take the agent-content migration lock: %w", err)
	}
	return lock, nil
}

func agentContentMigrationsDir(wsRoot string) string {
	return filepath.Join(wsRoot, ".putnami", agentContentMigrationDirName)
}

func agentContentJournalDir(wsRoot, name string) string {
	return filepath.Join(agentContentMigrationsDir(wsRoot), layout.EncodeName(name))
}

// checkJournalDirectories requires every directory from the workspace root to
// dir to be a real directory, creating the missing ones when create is set.
// The journal authorizes writes and removals in the workspace, so it is never
// read or written through a symlink.
func checkJournalDirectories(wsRoot, dir string, create bool) error {
	rel, err := filepath.Rel(wsRoot, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is outside the workspace", dir)
	}
	current := filepath.Clean(wsRoot)
	for _, component := range strings.Split(rel, string(os.PathSeparator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink, which is never followed", current)
		case err == nil && !info.IsDir():
			return fmt.Errorf("%s is not a directory", current)
		case err == nil:
			continue
		case !os.IsNotExist(err):
			return fmt.Errorf("inspect %s: %w", current, err)
		case !create:
			return nil
		}
		if err := os.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create %s: %w", current, err)
		}
	}
	return nil
}

// listAgentContentJournals reads every journal in the workspace, sorted by
// extension. A directory without a journal document is a start that never
// wrote one and is ignored; an unreadable journal fails closed.
func listAgentContentJournals(wsRoot string) ([]*agentContentJournal, error) {
	dir := agentContentMigrationsDir(wsRoot)
	if err := checkJournalDirectories(wsRoot, dir, false); err != nil {
		return nil, corruptJournal(dir, err.Error())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, corruptJournal(dir, fmt.Sprintf("cannot be listed (%v)", err))
	}
	var journals []*agentContentJournal
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		journal, err := readAgentContentJournal(wsRoot, filepath.Join(dir, entry.Name()), "")
		if err != nil {
			return nil, err
		}
		if journal == nil {
			continue
		}
		if layout.EncodeName(journal.Extension) != entry.Name() {
			return nil, corruptJournal(filepath.Join(dir, entry.Name(), agentContentJournalFilename),
				fmt.Sprintf("records extension %q, which this directory does not belong to", journal.Extension))
		}
		journals = append(journals, journal)
	}
	sort.Slice(journals, func(i, j int) bool { return journals[i].Extension < journals[j].Extension })
	return journals, nil
}

func loadAgentContentJournal(wsRoot, name string) (*agentContentJournal, error) {
	return readAgentContentJournal(wsRoot, agentContentJournalDir(wsRoot, name), name)
}

// readAgentContentJournal strictly decodes and validates one journal. A
// missing document returns nil.
func readAgentContentJournal(wsRoot, dir, name string) (*agentContentJournal, error) {
	path := filepath.Join(dir, agentContentJournalFilename)
	if err := checkJournalDirectories(wsRoot, dir, false); err != nil {
		return nil, corruptJournal(path, err.Error())
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, corruptJournal(path, fmt.Sprintf("cannot be inspected (%v)", err))
	}
	if !info.Mode().IsRegular() {
		return nil, corruptJournal(path, "is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, corruptJournal(path, fmt.Sprintf("cannot be read (%v)", err))
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var journal agentContentJournal
	if err := dec.Decode(&journal); err != nil {
		return nil, corruptJournal(path, fmt.Sprintf("is not a valid journal (%v)", err))
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, corruptJournal(path, "carries trailing data")
	}
	if reason := validateAgentContentJournal(&journal, name); reason != "" {
		return nil, corruptJournal(path, reason)
	}
	return &journal, nil
}

// validateAgentContentJournal returns why a journal cannot be trusted, or "".
// Every path it names authorizes a write or a removal, so each one is held to
// the protocol's own path rule.
func validateAgentContentJournal(journal *agentContentJournal, name string) string {
	for _, check := range []func() string{
		func() string { return validateJournalHeader(journal, name) },
		func() string { return validateJournalState(journal.After) },
		func() string { return validateJournalState(journal.Target) },
		func() string { return validateJournalSuperseded(journal) },
		func() string { return validateJournalPaths(journal) },
	} {
		if reason := check(); reason != "" {
			return reason
		}
	}
	return ""
}

// validateJournalHeader checks the journal's identity, phase and the digests
// that bind it to one content release and one workspace config.
func validateJournalHeader(journal *agentContentJournal, name string) string {
	switch {
	case journal.Version != agentContentJournalVersion:
		return fmt.Sprintf("records journal version %d, but this putnami reads version %d", journal.Version, agentContentJournalVersion)
	case name != "" && journal.Extension != name:
		return fmt.Sprintf("records extension %q, but %q was requested", journal.Extension, name)
	case !extensionNameFormat.MatchString(journal.Extension):
		return fmt.Sprintf("records an invalid extension name %q", journal.Extension)
	case journal.Phase != journalApplying && journal.Phase != journalApplied && journal.Phase != journalRollingBack:
		return fmt.Sprintf("records an unknown phase %q", journal.Phase)
	case strings.TrimSpace(journal.Content.Version) == "" || !isHexDigest(journal.Content.ArchiveDigest) || !isHexDigest(journal.Content.ManifestHash):
		return "records no valid content identity"
	case !isHexDigest(journal.Config.Before) || !isHexDigest(journal.Config.After):
		return "records no valid workspace config digests"
	case len(journal.Superseded) == 0:
		return "records no superseded artifact"
	case journal.After == nil || journal.After.Name != journal.Extension:
		return "records no ownership record for the extension"
	case journal.Target != nil && journal.Target.Name != journal.Extension:
		return "records a previous ownership record for another artifact"
	case len(journal.Declarations.After) == 0:
		return "records no declarations after the migration"
	}
	return ""
}

// validateJournalSuperseded checks each superseded artifact's recorded state.
func validateJournalSuperseded(journal *agentContentJournal) string {
	seen := make(map[string]bool, len(journal.Superseded))
	for _, item := range journal.Superseded {
		if item.Name == journal.Extension || seen[item.Name] || !extensionNameFormat.MatchString(item.Name) {
			return fmt.Sprintf("records an invalid superseded artifact %q", item.Name)
		}
		seen[item.Name] = true
		if item.Record != nil && item.Record.Name != item.Name {
			return fmt.Sprintf("records the ownership of %q under %q", item.Record.Name, item.Name)
		}
		if reason := validateJournalState(item.Record); reason != "" {
			return reason
		}
		if reason := validateJournalFiles(item.History); reason != "" {
			return reason
		}
		if item.Pin != nil {
			if err := agentartifacts.ValidatePin(item.Name, *item.Pin); err != nil {
				return err.Error()
			}
		}
	}
	return ""
}

// validateJournalPaths checks every path the journal would write, remove or
// release.
func validateJournalPaths(journal *agentContentJournal) string {
	for _, file := range journal.Files {
		if !wsproto.ValidAgentArtifactPath(file.Path) {
			return fmt.Sprintf("records an unsafe path %q", file.Path)
		}
		if (file.Before == "" && file.After == "") ||
			(file.Before != "" && !isHexDigest(file.Before)) || (file.After != "" && !isHexDigest(file.After)) {
			return fmt.Sprintf("records malformed digests for %q", file.Path)
		}
	}
	for _, path := range journal.Released {
		if !wsproto.ValidAgentArtifactPath(path) {
			return fmt.Sprintf("records an unsafe path %q", path)
		}
	}
	return ""
}

func validateJournalState(state *agentartifacts.State) string {
	if state == nil {
		return ""
	}
	data, err := agentartifacts.MarshalState(state)
	if err != nil {
		return err.Error()
	}
	if _, err := agentartifacts.ParseState(data, state.Name); err != nil {
		return err.Error()
	}
	return ""
}

func validateJournalFiles(files []agentartifacts.File) string {
	for _, file := range files {
		if !wsproto.ValidAgentArtifactPath(file.Path) || !isHexDigest(file.SHA256) {
			return fmt.Sprintf("records an unsafe or malformed file %q", file.Path)
		}
	}
	return ""
}

// startAgentContentJournal writes a fresh journal: the copies of every file
// the migration replaces or removes first, the journal document last and
// atomically, so a journal that exists always has its copies. It clears what
// a start that never wrote its document left behind; a journal that exists is
// never replaced, because prepareAgentContentMigration resumes an unfinished
// one and refuses to start while a completed one keeps its rollback point.
func startAgentContentJournal(wsRoot string, journal *agentContentJournal) error {
	dir := agentContentJournalDir(wsRoot, journal.Extension)
	if err := checkJournalDirectories(wsRoot, agentContentMigrationsDir(wsRoot), true); err != nil {
		return err
	}
	if err := checkJournalDirectories(wsRoot, dir, false); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	written := false
	defer func() {
		if !written {
			_ = os.RemoveAll(dir)
		}
	}()
	before := filepath.Join(dir, agentContentJournalBeforeDir)
	if err := checkJournalDirectories(wsRoot, before, true); err != nil {
		return err
	}
	config, err := os.ReadFile(filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename))
	if err != nil {
		return err
	}
	if got := lockfile.HashBytes(config); got != journal.Config.Before {
		return fmt.Errorf("%s changed while the migration was planned", wsproto.WorkspaceConfigFilename)
	}
	if err := os.WriteFile(filepath.Join(dir, agentContentJournalConfig), config, 0o644); err != nil {
		return err
	}
	for _, file := range journal.Files {
		if file.Before == "" {
			continue
		}
		content, err := readRegularWorkspaceFile(wsRoot, file.Path)
		if err != nil {
			return err
		}
		if got := lockfile.HashBytes(content); got != file.Before {
			return fmt.Errorf("%s changed while the migration was planned (now %s)", file.Path, got)
		}
		target := filepath.Join(before, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return err
		}
	}
	if err := saveAgentContentJournal(wsRoot, journal); err != nil {
		return err
	}
	written = true
	return nil
}

func readRegularWorkspaceFile(wsRoot, rel string) ([]byte, error) {
	if !wsproto.ValidAgentArtifactPath(rel) {
		return nil, fmt.Errorf("refusing to read unsafe path %q", rel)
	}
	path := filepath.Join(wsRoot, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return os.ReadFile(path)
}

// saveAgentContentJournal publishes the journal document atomically.
func saveAgentContentJournal(wsRoot string, journal *agentContentJournal) error {
	dir := agentContentJournalDir(wsRoot, journal.Extension)
	if err := checkJournalDirectories(wsRoot, dir, true); err != nil {
		return err
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return shared.AtomicWriteFile(filepath.Join(dir, agentContentJournalFilename), append(data, '\n'))
}

func removeAgentContentJournal(wsRoot, name string) error {
	dir := agentContentJournalDir(wsRoot, name)
	if err := checkJournalDirectories(wsRoot, dir, false); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// The parent is pruned only when this journal was its last entry.
	_ = os.Remove(agentContentMigrationsDir(wsRoot))
	return nil
}

func corruptJournal(path, reason string) error {
	return fmt.Errorf(
		"agent-content migration journal %s is unusable: %s. Nothing was changed. Remove its directory to discard it; the migration it records can then no longer be resumed or rolled back",
		path, reason)
}
