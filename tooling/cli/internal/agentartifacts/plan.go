package agentartifacts

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// Action is what the materializer would do to one path. The five values are
// exactly the five report sections, so a plan entry and a report line can never
// disagree about a path's fate.
type Action string

// The classification vocabulary. These strings are part of the machine report,
// so they are stable identifiers, not display text.
const (
	// ActionCreate: the target does not exist.
	ActionCreate Action = "create"
	// ActionUpdate: the target exists, is recorded as managed, and still holds
	// exactly the bytes that were recorded — so replacing it destroys nothing.
	ActionUpdate Action = "update"
	// ActionRemove: the artifact dropped the path, and the target still holds
	// exactly the bytes that were recorded.
	ActionRemove Action = "remove"
	// ActionUnchanged: the target already holds the artifact's bytes.
	ActionUnchanged Action = "unchanged"
	// ActionCollide: the target is, or may be, user-owned. It is preserved.
	ActionCollide Action = "collision"
)

// Collision reasons. Each names a distinct way a path fails to be provably
// ours, and each maps to one row of the ownership table (or to one filesystem
// shape that makes the question unanswerable).
const (
	// ReasonUnmanaged: the file exists, differs from the artifact, and no
	// previous install recorded it. It is somebody else's file.
	ReasonUnmanaged = "unmanaged"
	// ReasonModified: recorded as managed, but its bytes are no longer the
	// recorded ones — the user edited what we installed.
	ReasonModified = "modified"
	// ReasonModifiedRemoval: the artifact dropped this path, but the user edited
	// it since it was installed, so the removal is no longer proven safe.
	ReasonModifiedRemoval = "modified-removal"
	// ReasonSymlink: the target, or a directory on the way to it, is a symlink.
	// Writing through it would place bytes wherever the link points.
	ReasonSymlink = "symlink"
	// ReasonNotAFile: the target exists and is not a regular file.
	ReasonNotAFile = "not-a-file"
	// ReasonNotADirectory: a parent component of the target exists and is not a
	// directory, so the target cannot be created without destroying it.
	ReasonNotADirectory = "not-a-directory"
	// ReasonUnreadable: the target cannot be inspected, so ownership is
	// undecidable. Undecidable resolves to "preserve", never to "overwrite".
	ReasonUnreadable = "unreadable"
)

// Entry is one classified target.
type Entry struct {
	// Path is the workspace-relative, slash-separated target path.
	Path string
	// Action is the classification.
	Action Action
	// Reason is the collision reason; empty for every other action.
	Reason string
	// Detail explains a collision in one sentence; empty otherwise.
	Detail string
	// SHA256 is the artifact digest for create/update/unchanged, and the
	// recorded digest for remove. Empty for collisions, whose whole point is
	// that no digest authorizes the write.
	SHA256 string
}

// Plan is the COMPLETE preflight classification: every path the new artifact
// declares plus every path the previous install recorded. It is built entirely
// before any mutation, which is what makes "one collision aborts everything"
// implementable rather than aspirational.
type Plan struct {
	Name          string
	Version       string
	ArchiveDigest string
	ManifestHash  string
	// Entries are sorted by path, then by action, so a plan is comparable
	// across runs and across machines.
	Entries []Entry
}

// BuildPlan classifies every target against the ownership table. It reads the
// workspace but never writes to it.
//
// The table, in the order the branches appear below:
//
//	missing                                   -> create
//	byte-identical to the artifact            -> unchanged
//	recorded managed AND still recorded bytes -> update
//	recorded managed AND locally modified     -> collision (modified)
//	present but unrecorded                    -> collision (unmanaged)
//	dropped, still the recorded bytes         -> remove
//	dropped, locally modified                 -> collision (modified-removal)
//
// "byte-identical" is checked BEFORE ownership on purpose: a path whose content
// already equals the artifact's needs no write and no ownership proof, which is
// also what makes an interrupted run safe to rerun — the files it did write
// come back as unchanged rather than as unrecorded collisions.
func BuildPlan(workspaceRoot string, artifact *Artifact, state *State) (*Plan, error) {
	if artifact == nil {
		return nil, fmt.Errorf("cannot plan a nil agent artifact")
	}
	if state != nil && state.Name != artifact.Name {
		return nil, fmt.Errorf("ownership state records artifact %q, cannot plan %q", state.Name, artifact.Name)
	}

	recorded := state.ManagedDigests()
	declared := artifact.Digests()

	entries := make([]Entry, 0, len(declared)+len(recorded))
	for _, file := range artifact.Files {
		entry, err := classifyDeclared(workspaceRoot, file, recorded)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	dropped := make([]string, 0, len(recorded))
	for path := range recorded {
		if _, still := declared[path]; !still {
			dropped = append(dropped, path)
		}
	}
	sort.Strings(dropped)
	for _, path := range dropped {
		entry, keep, err := classifyDropped(workspaceRoot, path, recorded[path])
		if err != nil {
			return nil, err
		}
		if keep {
			entries = append(entries, entry)
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].Action < entries[j].Action
	})

	return &Plan{
		Name:          artifact.Name,
		Version:       artifact.Version,
		ArchiveDigest: artifact.ArchiveDigest,
		ManifestHash:  artifact.ManifestHash,
		Entries:       entries,
	}, nil
}

// classifyDeclared decides the fate of one path the new artifact declares.
func classifyDeclared(workspaceRoot string, file File, recorded map[string]string) (Entry, error) {
	target, err := workspacePath(workspaceRoot, file.Path)
	if err != nil {
		return Entry{}, err
	}
	if reason, detail, bad := inspectParents(workspaceRoot, file.Path); bad {
		return collision(file.Path, reason, detail), nil
	}

	info, err := os.Lstat(target)
	switch {
	case err == nil:
		// fall through to the shape and content checks below
	case os.IsNotExist(err):
		return Entry{Path: file.Path, Action: ActionCreate, SHA256: file.SHA256}, nil
	default:
		return collision(file.Path, ReasonUnreadable, fmt.Sprintf("cannot be inspected (%v)", err)), nil
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return collision(file.Path, ReasonSymlink,
			"the target is a symlink; writing through it would place bytes outside the path the artifact declares"), nil
	}
	if !info.Mode().IsRegular() {
		return collision(file.Path, ReasonNotAFile, "the target exists and is not a regular file"), nil
	}

	content, err := os.ReadFile(target)
	if err != nil {
		return collision(file.Path, ReasonUnreadable, fmt.Sprintf("cannot be read (%v)", err)), nil
	}
	onDisk := lockfile.HashBytes(content)
	if onDisk == file.SHA256 {
		return Entry{Path: file.Path, Action: ActionUnchanged, SHA256: file.SHA256}, nil
	}
	managed, ok := recorded[file.Path]
	switch {
	case ok && managed == onDisk:
		return Entry{Path: file.Path, Action: ActionUpdate, SHA256: file.SHA256}, nil
	case ok:
		return collision(file.Path, ReasonModified,
			"the file was installed by a previous run and has been edited since; the edit would be lost"), nil
	default:
		return collision(file.Path, ReasonUnmanaged,
			"the file exists but no previous run recorded installing it, so it is user-owned"), nil
	}
}

// classifyDropped decides the fate of one path the previous install recorded
// and the new artifact no longer declares. keep is false when the path is
// already absent: nothing is mutated, so it belongs in no report section.
func classifyDropped(workspaceRoot, path, managed string) (entry Entry, keep bool, err error) {
	target, err := workspacePath(workspaceRoot, path)
	if err != nil {
		return Entry{}, false, err
	}
	if reason, detail, bad := inspectParents(workspaceRoot, path); bad {
		return collision(path, reason, detail), true, nil
	}

	info, err := os.Lstat(target)
	switch {
	case err == nil:
		// fall through
	case os.IsNotExist(err):
		return Entry{}, false, nil // already gone: nothing to remove, nothing to report
	default:
		return collision(path, ReasonUnreadable, fmt.Sprintf("cannot be inspected (%v)", err)), true, nil
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return collision(path, ReasonSymlink,
			"a managed path was replaced by a symlink; removing it is no longer the removal that was recorded"), true, nil
	}
	if !info.Mode().IsRegular() {
		return collision(path, ReasonNotAFile, "a managed path was replaced by something that is not a regular file"), true, nil
	}

	content, err := os.ReadFile(target)
	if err != nil {
		return collision(path, ReasonUnreadable, fmt.Sprintf("cannot be read (%v)", err)), true, nil
	}
	if lockfile.HashBytes(content) != managed {
		return collision(path, ReasonModifiedRemoval,
			"the artifact no longer ships this file, but it has been edited since it was installed"), true, nil
	}
	return Entry{Path: path, Action: ActionRemove, SHA256: managed}, true, nil
}

// inspectParents walks the directory components between the workspace root and
// path, and reports the first one that makes the target unsafe to touch.
//
// This runs in PREFLIGHT rather than at write time for two reasons. A parent
// that exists as a file makes the write fail halfway through the mutation,
// which is the one thing the transaction promises cannot happen; and a parent
// that is a SYMLINK makes the write succeed at the wrong place, which is worse.
// Rejecting a symlinked parent is why the symlink rule covers destinations and
// not only manifest-declared paths.
func inspectParents(workspaceRoot, path string) (reason, detail string, bad bool) {
	components := strings.Split(path, "/")
	current := workspaceRoot
	for _, component := range components[:len(components)-1] {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return "", "", false // the rest of the chain does not exist either
			}
			return ReasonUnreadable, fmt.Sprintf("parent %s cannot be inspected (%v)", current, err), true
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ReasonSymlink,
				fmt.Sprintf("parent directory %s is a symlink, which is never followed for a write", component), true
		}
		if !info.IsDir() {
			return ReasonNotADirectory,
				fmt.Sprintf("parent path %s exists and is not a directory", component), true
		}
	}
	return "", "", false
}

// workspacePath resolves a declared workspace-relative path to an absolute one,
// re-validating it against the protocol's exported rule first. Every caller
// that turns a recorded or declared path into a filesystem operation goes
// through here, so there is exactly one place where a string becomes a path.
func workspacePath(workspaceRoot, path string) (string, error) {
	if !wsproto.ValidAgentArtifactPath(path) {
		return "", fmt.Errorf("refusing to touch unsafe agent-artifact path %q", path)
	}
	return filepath.Join(workspaceRoot, filepath.FromSlash(path)), nil
}

func collision(path, reason, detail string) Entry {
	return Entry{Path: path, Action: ActionCollide, Reason: reason, Detail: detail}
}

// Select returns the entries carrying one action, in plan order.
func (p *Plan) Select(action Action) []Entry {
	out := make([]Entry, 0, len(p.Entries))
	for _, entry := range p.Entries {
		if entry.Action == action {
			out = append(out, entry)
		}
	}
	return out
}

// Collisions returns the collided entries in plan order.
func (p *Plan) Collisions() []Entry { return p.Select(ActionCollide) }

// HasCollisions reports whether any target is, or may be, user-owned. It is the
// single gate in front of every mutation.
func (p *Plan) HasCollisions() bool {
	for _, entry := range p.Entries {
		if entry.Action == ActionCollide {
			return true
		}
	}
	return false
}

// Mutates reports whether applying the plan would change the workspace at all.
func (p *Plan) Mutates() bool {
	for _, entry := range p.Entries {
		if entry.Action == ActionCreate || entry.Action == ActionUpdate || entry.Action == ActionRemove {
			return true
		}
	}
	return false
}
