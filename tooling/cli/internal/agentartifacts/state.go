package agentartifacts

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
	"go.putnami.dev/tooling/cli/internal/layout"
)

// StateDirName is the gitignored directory under .putnami/ that holds
// agent-artifact ownership bookkeeping. .putnami/ is already ignored, so the
// state never enters a commit and a fresh checkout legitimately starts without
// it.
const StateDirName = "agent-artifacts"

// StateVersion is the ownership-state schema version. It is compared exactly:
// a state written by a different schema is CORRUPT, not stale-but-usable,
// because the one thing this file exists to answer — "did we write these exact
// bytes?" — cannot be guessed from a shape we do not know.
const StateVersion = 1

// State records what a previous successful materialization installed: the
// artifact identity and digests, and every managed path with the digest of the
// bytes that were written to it.
//
// The per-file digest is the whole point. Recording only paths would make
// "locally modified" undecidable without re-fetching the previously installed
// artifact — which is exactly the situation where a materializer starts
// guessing, and guessing here means overwriting a user's edit.
//
// The shape carries NO timestamp. A wall-clock field would make two identical
// installs produce different bytes, and this file is compared, diffed, and
// asserted on.
type State struct {
	Version         int    `json:"version"`
	Name            string `json:"name"`
	ArtifactVersion string `json:"artifactVersion"`
	ArchiveDigest   string `json:"archiveDigest"`
	ManifestHash    string `json:"manifestHash"`
	Files           []File `json:"files"`
}

// CorruptStateError reports ownership state that cannot be trusted to answer
// the ownership question. It is a hard failure with a remediation, never a
// silent fallback to "nothing is managed": the two readings differ by exactly
// the user files a wrong guess would delete or overwrite.
type CorruptStateError struct {
	// Path is the state file that could not be trusted.
	Path string
	// Reason states what is wrong, in the terms of the file's contract.
	Reason string
}

func (e *CorruptStateError) Error() string {
	return fmt.Sprintf(
		"agent-artifact ownership state %s is unusable: %s. "+
			"Nothing was changed. Remove the ownership state file or invalid state directory to re-adopt this workspace: every path it covered is then treated as user-owned, "+
			"preserved, and reported as a collision instead of being overwritten or removed",
		e.Path, e.Reason)
}

// StatePath returns the ownership-state file for one artifact name.
func StatePath(workspaceRoot, name string) string {
	return filepath.Join(stateDirectory(workspaceRoot), layout.EncodeName(name)+".json")
}

func stateDirectory(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".putnami", StateDirName)
}

// checkStateDirectory ensures that every state-directory parent is a real
// directory. State grants authority to remove managed files, so neither its
// file nor the directories leading to it may be followed through a symlink.
//
// When create is false, missing directories are fine: they mean there is no
// state yet. When create is true, each missing directory is made and then
// inspected before continuing, so WriteState never creates a file outside the
// workspace through a pre-existing link.
func checkStateDirectory(workspaceRoot string, create bool) error {
	path := filepath.Clean(workspaceRoot)
	for _, element := range []string{".putnami", StateDirName} {
		path = filepath.Join(path, element)
		if err := checkStateDirectoryComponent(path, create); err != nil {
			return err
		}
	}
	return nil
}

func checkStateDirectoryComponent(path string, create bool) error {
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symlink, which is never followed", path)
			}
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", path)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect directory %s: %w", path, err)
		}
		if !create {
			return nil
		}
		if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create directory %s: %w", path, err)
		}
	}
}

// LoadState reads the ownership state for name.
//
// A MISSING file returns (nil, nil): that is a fresh checkout or a workspace
// that never installed this artifact, which is a legitimate starting state and
// not an error — the same rule the install marker uses. Every other failure —
// unreadable, unparseable, wrong schema version, wrong identity, or a record
// this package would refuse to write — is a *CorruptStateError.
func LoadState(workspaceRoot, name string) (*State, error) {
	path := StatePath(workspaceRoot, name)
	if err := checkStateDirectory(workspaceRoot, false); err != nil {
		return nil, &CorruptStateError{Path: path, Reason: fmt.Sprintf("state directory is unusable (%v)", err)}
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &CorruptStateError{Path: path, Reason: fmt.Sprintf("cannot be inspected (%v)", err)}
	}
	// The state authorizes deletions, so it is never read through a symlink: a
	// link here would let anything that can create one choose which file this
	// package believes it owns.
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, &CorruptStateError{Path: path, Reason: "is a symlink, which is never followed"}
	}
	if !info.Mode().IsRegular() {
		return nil, &CorruptStateError{Path: path, Reason: "is not a regular file"}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &CorruptStateError{Path: path, Reason: fmt.Sprintf("cannot be read (%v)", err)}
	}
	state, err := ParseState(data, name)
	if err != nil {
		var corrupt *CorruptStateError
		if errors.As(err, &corrupt) {
			corrupt.Path = path
			return nil, corrupt
		}
		return nil, err
	}
	return state, nil
}

// ParseState strictly decodes and validates ownership-state bytes. Unknown
// fields and trailing documents are rejected for the same reason the manifest
// parser rejects them: every accepted byte must have a defined meaning, and a
// field this reader silently drops is a field a future reader would act on.
func ParseState(data []byte, name string) (*State, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var state State
	if err := dec.Decode(&state); err != nil {
		return nil, &CorruptStateError{Reason: fmt.Sprintf("is not a valid ownership document (%v)", err)}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, &CorruptStateError{Reason: "carries trailing data after the ownership document"}
	}

	if state.Version != StateVersion {
		return nil, &CorruptStateError{
			Reason: fmt.Sprintf("records schema version %d, but this putnami writes version %d", state.Version, StateVersion),
		}
	}
	if state.Name != name {
		return nil, &CorruptStateError{
			Reason: fmt.Sprintf("records artifact %q, but %q was requested", state.Name, name),
		}
	}
	if strings.TrimSpace(state.ArtifactVersion) == "" {
		return nil, &CorruptStateError{Reason: "records no artifact version"}
	}
	if !isSHA256Hex(state.ArchiveDigest) {
		return nil, &CorruptStateError{Reason: fmt.Sprintf("records a malformed archive digest %q", state.ArchiveDigest)}
	}
	if !isSHA256Hex(state.ManifestHash) {
		return nil, &CorruptStateError{Reason: fmt.Sprintf("records a malformed manifest hash %q", state.ManifestHash)}
	}
	if len(state.Files) == 0 {
		return nil, &CorruptStateError{Reason: "records no managed files"}
	}

	seen := make(map[string]bool, len(state.Files))
	for _, file := range state.Files {
		// A recorded path is an authorization to DELETE. Re-check it against the
		// protocol's exported rule so a hand-edited or corrupted record can never
		// aim a removal outside the workspace.
		if !wsproto.ValidAgentArtifactPath(file.Path) {
			return nil, &CorruptStateError{Reason: fmt.Sprintf("records an unsafe managed path %q", file.Path)}
		}
		if !isSHA256Hex(file.SHA256) {
			return nil, &CorruptStateError{Reason: fmt.Sprintf("records a malformed digest for %q", file.Path)}
		}
		if seen[file.Path] {
			return nil, &CorruptStateError{Reason: fmt.Sprintf("records %q more than once", file.Path)}
		}
		seen[file.Path] = true
	}

	state.Files = sortedFiles(state.Files)
	return &state, nil
}

// StateFor builds the state a successful materialization of artifact records:
// every declared file is managed, with the digest that was written to it.
func StateFor(artifact *Artifact) *State {
	return &State{
		Version:         StateVersion,
		Name:            artifact.Name,
		ArtifactVersion: artifact.Version,
		ArchiveDigest:   artifact.ArchiveDigest,
		ManifestHash:    artifact.ManifestHash,
		Files:           sortedFiles(artifact.Files),
	}
}

// MarshalState renders the canonical bytes: files sorted by path, two-space
// indentation, exactly one trailing newline. Determinism is structural — the
// only collection is a slice this function sorts — so two identical installs
// produce byte-identical state.
func MarshalState(state *State) ([]byte, error) {
	out := *state
	out.Files = sortedFiles(state.Files)
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal agent-artifact ownership state: %w", err)
	}
	return append(data, '\n'), nil
}

// WriteState atomically publishes the ownership state (temp file + rename in
// the destination directory, never a rewrite in place), so an interrupted write
// leaves the PREVIOUS state intact rather than a truncated one. A truncated
// state would read as corrupt and block the next run — recoverable, but only
// after a human deletes it.
func WriteState(workspaceRoot string, state *State) error {
	data, err := MarshalState(state)
	if err != nil {
		return err
	}
	path := StatePath(workspaceRoot, state.Name)
	if err := checkStateDirectory(workspaceRoot, true); err != nil {
		return fmt.Errorf("prepare agent-artifact state dir: %w", err)
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("stage agent-artifact ownership state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("publish agent-artifact ownership state: %w", err)
	}
	return nil
}

// ManagedDigests returns the recorded path→digest map. A nil state yields an
// empty map, which is what makes "no state yet" behave as "nothing is proven
// managed" everywhere downstream without a nil check per call site.
func (s *State) ManagedDigests() map[string]string {
	if s == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(s.Files))
	for _, file := range s.Files {
		out[file.Path] = file.SHA256
	}
	return out
}

// sortedFiles returns a copy sorted by path, leaving the input untouched.
func sortedFiles(files []File) []File {
	out := append([]File(nil), files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
