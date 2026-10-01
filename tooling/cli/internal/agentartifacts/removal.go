package agentartifacts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Removing an artifact the workspace no longer declares is the one mutation
// with no new artifact to plan against: the only authority is the ownership
// state a previous run recorded. It reuses the dropped-path rule BuildPlan
// already applies to a file an upgrade stops shipping, and differs from an
// upgrade in exactly one place — an edited file does not block the run.
//
// An upgrade that would overwrite an edit must stop, because the user asked
// for the artifact and the edit would be lost. A removal asked for the
// opposite: the user no longer wants the artifact, and leaving their edited
// file in place loses nothing. So an edited file is released — preserved,
// reported, and no longer recorded — rather than turned into a collision that
// would make every later upgrade fail until the user cleaned it up by hand.

// BuildRemovalPlan classifies every path state records for removal. Unchanged
// managed files are removed; an edited, replaced, or unreadable one is a
// collision entry the caller releases instead of applying; an already absent
// one is omitted.
func BuildRemovalPlan(workspaceRoot string, state *State) (*Plan, error) {
	if state == nil {
		return nil, fmt.Errorf("cannot plan the removal of an artifact with no ownership state")
	}
	entries := make([]Entry, 0, len(state.Files))
	for _, file := range state.Files {
		entry, keep, err := classifyDropped(workspaceRoot, file.Path, file.SHA256)
		if err != nil {
			return nil, err
		}
		if keep {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return &Plan{
		Name:          state.Name,
		Version:       state.ArtifactVersion,
		ArchiveDigest: state.ArchiveDigest,
		ManifestHash:  state.ManifestHash,
		Entries:       entries,
	}, nil
}

// ApplyRemoval deletes the plan's removable files and returns the paths it
// released. It never writes a byte, so it needs no artifact tree.
func ApplyRemoval(workspaceRoot string, plan *Plan) ([]string, error) {
	removable := &Plan{Name: plan.Name, Version: plan.Version, Entries: plan.Select(ActionRemove)}
	if err := Apply(workspaceRoot, &Artifact{Name: plan.Name, Version: plan.Version}, removable); err != nil {
		return nil, err
	}
	released := make([]string, 0)
	for _, entry := range plan.Collisions() {
		released = append(released, entry.Path)
	}
	return released, nil
}

// ManagedPaths is every workspace-relative path an ownership state records.
// A workspace without states, or with one that cannot be read, manages none:
// a caller that only describes a change never fails on a corrupt state.
func ManagedPaths(workspaceRoot string) map[string]bool {
	states, err := ListStates(workspaceRoot)
	if err != nil {
		return nil
	}
	managed := map[string]bool{}
	for _, state := range states {
		for _, file := range state.Files {
			managed[file.Path] = true
		}
	}
	return managed
}

// ListStates returns every ownership state recorded in the workspace, sorted
// by artifact name. Each file is validated exactly as LoadState validates it,
// and a file whose recorded name does not map back to its own file name is
// corrupt: that mismatch is how a renamed or hand-copied record would claim
// files it never installed.
func ListStates(workspaceRoot string) ([]*State, error) {
	dir := stateDirectory(workspaceRoot)
	if err := checkStateDirectory(workspaceRoot, false); err != nil {
		return nil, &CorruptStateError{Path: dir, Reason: fmt.Sprintf("state directory is unusable (%v)", err)}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &CorruptStateError{Path: dir, Reason: fmt.Sprintf("cannot be listed (%v)", err)}
	}
	states := make([]*State, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		// A staged write that was interrupted is not a record.
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.Contains(name, ".tmp.") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &CorruptStateError{Path: path, Reason: fmt.Sprintf("cannot be read (%v)", err)}
		}
		var identity struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(bytes.NewReader(data)).Decode(&identity); err != nil || identity.Name == "" {
			return nil, &CorruptStateError{Path: path, Reason: "records no artifact name"}
		}
		if StatePath(workspaceRoot, identity.Name) != path {
			return nil, &CorruptStateError{Path: path, Reason: fmt.Sprintf("records artifact %q, which is not the artifact this file name belongs to", identity.Name)}
		}
		state, err := LoadState(workspaceRoot, identity.Name)
		if err != nil {
			return nil, err
		}
		if state != nil {
			states = append(states, state)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	return states, nil
}

// RemoveState deletes the ownership record for name. It is the last step of a
// removal: once it is gone, nothing in the workspace is recorded as managed by
// that artifact, so it must follow every file removal and the lock update.
func RemoveState(workspaceRoot, name string) error {
	if err := checkStateDirectory(workspaceRoot, false); err != nil {
		return fmt.Errorf("remove agent-artifact ownership state: %w", err)
	}
	if err := os.Remove(StatePath(workspaceRoot, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove agent-artifact ownership state: %w", err)
	}
	return nil
}
