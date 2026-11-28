package workspace_state

import (
	"fmt"
	"strings"

	"go.putnami.dev/tooling/cli/internal/cacheprovider"
)

// SettledMarker is a run marker the cache provider settled at a commit, with
// the witness of the scope it named. While the witness holds, the provider's
// answer for that commit is known, and the run starts no provider to publish
// it again (tooling/cli ADR 0138).
type SettledMarker struct {
	SHA string `json:"sha"`
	// Workspace is the run-marker workspace identity the marker was published
	// under; Provider names the provider extension, its version and runtime.
	Workspace string                     `json:"workspace"`
	Provider  string                     `json:"provider"`
	Scope     cacheprovider.ScopeWitness `json:"scope"`
}

// NewSettledMarker is the settled marker of sha under the witness the
// provider's scope gave when it was read. It is false when no witness was
// taken or when the scope moved since.
func NewSettledMarker(sha, workspace, provider string, witness *cacheprovider.ScopeWitness) (*SettledMarker, bool) {
	if strings.TrimSpace(sha) == "" || strings.TrimSpace(workspace) == "" || strings.TrimSpace(provider) == "" || !witness.Holds() {
		return nil, false
	}
	return &SettledMarker{SHA: sha, Workspace: workspace, Provider: provider, Scope: *witness}, true
}

// Holds reports whether m is the settled marker of sha for the workspace and
// provider, with its scope's witness holding.
func (m *SettledMarker) Holds(sha, workspace, provider string) bool {
	return m != nil && m.SHA == sha && m.Workspace == workspace && m.Provider == provider && m.Scope.Holds()
}

// SettledMarker returns the marker the provider settled for the branch,
// command set and command parameters, or nil.
func (ss *SessionStore) SettledMarker(branch string, commands []string, params map[string]any) (*SettledMarker, error) {
	commandKey := lastBuildCommandKey(commands, params)
	if strings.TrimSpace(branch) == "" || commandKey == "" {
		return nil, nil
	}
	state, err := ss.readLastBuildState()
	if err != nil {
		return nil, err
	}
	return state.Branches[strings.TrimSpace(branch)][commandKey].Marker, nil
}

// RecordSettledMarker keeps marker on the entry of the recorded successful
// build of the branch, command set and command parameters.
func (ss *SessionStore) RecordSettledMarker(branch string, commands []string, params map[string]any, marker *SettledMarker) error {
	branch = strings.TrimSpace(branch)
	commandKey := lastBuildCommandKey(commands, params)
	if branch == "" || commandKey == "" || marker == nil {
		return fmt.Errorf("record settled run marker: empty branch, command set or marker")
	}
	state, err := ss.readLastBuildState()
	if err != nil {
		return err
	}
	entry, ok := state.Branches[branch][commandKey]
	if !ok {
		return fmt.Errorf("record settled run marker for %q: no successful build recorded", branch)
	}
	entry.Marker = marker
	state.Branches[branch][commandKey] = entry
	return ss.writeLastBuildState(state)
}
