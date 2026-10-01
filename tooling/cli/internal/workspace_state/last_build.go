package workspace_state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const lastBuildFilename = "last-build.json"

type lastBuildState struct {
	Version  int                                  `json:"version"`
	Branches map[string]map[string]lastBuildEntry `json:"branches"`
}

type lastBuildEntry struct {
	SHA       string `json:"sha"`
	UpdatedAt string `json:"updatedAt"`
}

// LastBuildSHA returns the last fully successful HEAD SHA recorded for the
// branch, command set, and command parameters.
func (ss *SessionStore) LastBuildSHA(branch string, commands []string, params map[string]any) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", nil
	}
	commandKey := lastBuildCommandKey(commands, params)
	if commandKey == "" {
		return "", nil
	}
	state, err := ss.readLastBuildState()
	if err != nil {
		return "", err
	}
	branchEntries, ok := state.Branches[branch]
	if !ok {
		return "", nil
	}
	entry, ok := branchEntries[commandKey]
	if !ok {
		return "", nil
	}
	return strings.TrimSpace(entry.SHA), nil
}

// RecordSuccessfulBuild records the current HEAD SHA for a fully successful
// non-watch run. The state is keyed by branch, command set, and command
// parameters, and scoped to this worktree.
func (ss *SessionStore) RecordSuccessfulBuild(branch string, commands []string, params map[string]any, sha string) error {
	branch = strings.TrimSpace(branch)
	sha = strings.TrimSpace(sha)
	if branch == "" {
		return fmt.Errorf("record last build: empty branch")
	}
	commandKey := lastBuildCommandKey(commands, params)
	if commandKey == "" {
		return fmt.Errorf("record last build for %q: empty command set", branch)
	}
	if sha == "" {
		return fmt.Errorf("record last build for %q: empty sha", branch)
	}

	state, err := ss.readLastBuildState()
	if err != nil {
		return err
	}
	state.Version = 2
	if state.Branches == nil {
		state.Branches = make(map[string]map[string]lastBuildEntry)
	}
	if state.Branches[branch] == nil {
		state.Branches[branch] = make(map[string]lastBuildEntry)
	}
	state.Branches[branch][commandKey] = lastBuildEntry{
		SHA:       sha,
		UpdatedAt: time.Now().Format(time.RFC3339Nano),
	}
	return ss.writeLastBuildState(state)
}

func (ss *SessionStore) lastBuildPath() string {
	return filepath.Join(ss.root, lastBuildFilename)
}

func (ss *SessionStore) readLastBuildState() (*lastBuildState, error) {
	data, err := os.ReadFile(ss.lastBuildPath())
	if err != nil {
		if os.IsNotExist(err) {
			return newLastBuildState(), nil
		}
		return nil, fmt.Errorf("read last build state: %w", err)
	}

	var state lastBuildState
	if err := json.Unmarshal(data, &state); err != nil {
		state, migrateErr := migrateLegacyLastBuildState(data)
		if migrateErr != nil {
			return nil, fmt.Errorf("read last build state: %w", err)
		}
		return state, nil
	}
	if state.Branches == nil {
		state.Branches = make(map[string]map[string]lastBuildEntry)
	}
	state.Version = 2
	return &state, nil
}

func newLastBuildState() *lastBuildState {
	return &lastBuildState{
		Version:  2,
		Branches: make(map[string]map[string]lastBuildEntry),
	}
}

func migrateLegacyLastBuildState(data []byte) (*lastBuildState, error) {
	var legacy struct {
		Branches map[string]lastBuildEntry `json:"branches"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}
	state := newLastBuildState()
	// Branch-only legacy entries were ambiguous across command sets, so do not
	// reuse them as a baseline. Return an empty v2 state so the next successful
	// command-specific run records a precise marker.
	return state, nil
}

func lastBuildCommandKey(commands []string, params map[string]any) string {
	normalized := make([]string, 0, len(commands))
	for _, cmd := range commands {
		cmd = strings.TrimSpace(cmd)
		if cmd != "" {
			normalized = append(normalized, cmd)
		}
	}
	if len(normalized) == 0 {
		return ""
	}
	commandKey := strings.Join(normalized, ",")
	paramsHash := LastBuildParamsHash(params)
	if paramsHash == "" {
		return commandKey
	}
	return commandKey + "@" + paramsHash
}

// LastBuildParamsHash returns the stable short hash used to distinguish local
// and remote run markers for the same command list with different job
// parameters. An empty map has no hash.
func LastBuildParamsHash(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	data, err := json.Marshal(params)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

func (ss *SessionStore) writeLastBuildState(state *lastBuildState) error {
	if err := os.MkdirAll(ss.root, 0o755); err != nil {
		return fmt.Errorf("create sessions dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal last build state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(ss.root, lastBuildFilename+".*.tmp")
	if err != nil {
		return fmt.Errorf("create last build temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write last build state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write last build state: %w", err)
	}
	if err := os.Rename(tmpPath, ss.lastBuildPath()); err != nil {
		return fmt.Errorf("replace last build state: %w", err)
	}
	return nil
}
