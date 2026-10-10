// Package gitcandidate reads a worktree the way a `git:` cache input keys it.
//
// A task input written `git:**` (ADR 0041 of the CLI) keys the task on the
// repository's candidate cut: every path `git ls-files --cached --others
// --exclude-standard` lists that exists on disk, a regular file by its bytes
// and its executable bit (ADR 0061 of the CLI) and a symbolic link by its
// target text. A cached verdict is sound only when
// the task reads nothing outside that cut, so a task that declares the input
// reads the worktree through a Tree: a path that is not a candidate does not
// exist for it, whatever the disk holds. Ignored build output, a file Git
// excludes and an empty directory are absent, as they are from a clone.
//
// A Tree resolves a symbolic link from its target text, segment by segment,
// and never follows one out of its root: the text is what the key holds, and
// what the link reaches is read only when it is a candidate itself.
package gitcandidate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/sourcebinding"
)

// Paths lists the candidate paths under root, slash-separated, relative to
// root, sorted and without duplicates: tracked paths plus untracked paths no
// ignore rule excludes. It runs the command the CLI enumerates a `git:` input
// with, so a task and its key list one set. A listed path may be absent from
// the disk (an unstaged deletion); Open drops it, as the key does.
func Paths(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("git ls-files did not terminate its NUL-delimited output")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			return nil, fmt.Errorf("git ls-files returned an empty candidate path")
		}
		paths = append(paths, string(part))
	}
	sort.Strings(paths)
	unique := paths[:0]
	for _, candidate := range paths {
		if len(unique) == 0 || unique[len(unique)-1] != candidate {
			unique = append(unique, candidate)
		}
	}
	return unique, nil
}

// indexModes is what the index records for the tracked paths under a root.
type indexModes struct {
	// Modes maps a tracked, merged path, slash-separated and relative to the
	// root, to its six-digit octal mode: "100644", "100755", "120000" or
	// "160000".
	Modes map[string]string
	// Unmerged holds every path the index lists at stage 1, 2 or 3.
	Unmerged map[string]bool
}

// readIndexModes reads the index under root once, with `git ls-files -z
// --stage`, as the CLI does to key a `git:` input, so that a Tree's
// Fingerprint reads the answer the key reads. A host that does not store the
// executable bit takes a tracked file's bit from its mode, as a source binding
// does (sourcebinding.FileMode), and an unmerged path has no single mode at
// all.
func readIndexModes(root string) (indexModes, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--stage")
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		return indexModes{}, fmt.Errorf("git ls-files --stage: %w", err)
	}
	return parseIndexModes(data)
}

// parseIndexModes reads `git ls-files -z --stage` output: one
// "<mode> <object> <stage>\t<path>" entry per NUL-terminated record.
func parseIndexModes(data []byte) (indexModes, error) {
	index := indexModes{Modes: map[string]string{}, Unmerged: map[string]bool{}}
	if len(data) == 0 {
		return index, nil
	}
	if data[len(data)-1] != 0 {
		return indexModes{}, fmt.Errorf("git ls-files --stage did not terminate its NUL-delimited output")
	}
	for _, record := range bytes.Split(data[:len(data)-1], []byte{0}) {
		metadata, name, found := bytes.Cut(record, []byte{'\t'})
		fields := bytes.Fields(metadata)
		if !found || len(name) == 0 || len(fields) != 3 {
			return indexModes{}, fmt.Errorf("git ls-files --stage returned a malformed entry %q", record)
		}
		if string(fields[2]) != "0" {
			index.Unmerged[string(name)] = true
			continue
		}
		index.Modes[string(name)] = string(fields[0])
	}
	return index, nil
}

// Kind is what a path names in a Tree.
type Kind int

const (
	// Missing is a path that is neither a candidate nor a directory holding
	// one, or a symbolic link whose target is missing.
	Missing Kind = iota
	// File is a regular candidate file, or a candidate symbolic link that
	// resolves to one.
	File
	// Dir is a directory that holds a candidate, or a candidate symbolic link
	// that resolves to one. The root is a directory.
	Dir
	// Outside is a path a symbolic link on its way leads out of the root.
	Outside
)

// maxLinkHops bounds symbolic link resolution, as the kernel bounds it: a
// cycle resolves to Missing instead of looping.
const maxLinkHops = 40

// Tree is the candidate cut under one root.
type Tree struct {
	root string
	// files holds every candidate that exists, by slash path relative to
	// root: true for a symbolic link, false for a regular file.
	files map[string]bool
	// dirs holds every directory that holds a candidate, and every candidate
	// Git lists as a directory (a submodule or a nested repository), whose
	// contents are not candidates.
	dirs map[string]bool
	// paths is the sorted keys of files.
	paths []string
	// unkeyable holds, sorted, every candidate a `git:` key refuses to hold:
	// one Git lists as a directory (a submodule or a nested repository, whose
	// checked-out commit no key reads) and a socket, a pipe or a device.
	unkeyable []string
	// links caches the target text of each symbolic link read.
	links map[string]string
}

// Open returns the candidate cut of root. It returns nil and no error when
// root is not inside a Git work tree, or when Git is not installed: no `git:`
// input can be keyed there, so the caller reads the disk as it always did. Any
// other failure to enumerate or inspect a candidate is an error, never an
// empty tree: an unreadable cut is not one with nothing in it.
func Open(root string) (*Tree, error) {
	root = filepath.Clean(root)
	paths, err := Paths(root)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || !insideWorkTree(root) {
			return nil, nil
		}
		return nil, err
	}
	return newTree(root, paths)
}

// insideWorkTree reports whether Git places dir inside a work tree.
func insideWorkTree(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// newTree inspects each listed path with os.Lstat, as the key does: an absent
// one is dropped, a regular file or a symbolic link is a candidate, and a
// directory (a submodule) is a directory whose contents are not candidates.
func newTree(root string, listed []string) (*Tree, error) {
	tree := &Tree{
		root:  root,
		files: make(map[string]bool, len(listed)),
		dirs:  map[string]bool{".": true},
		links: map[string]string{},
	}
	for _, candidate := range listed {
		rel := path.Clean(candidate)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
			return nil, fmt.Errorf("git ls-files listed %q, which is not inside %s", candidate, root)
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("git candidate %q: %w", candidate, err)
		}
		switch {
		case info.Mode().IsRegular():
			tree.files[rel] = false
		case info.Mode()&fs.ModeSymlink != 0:
			tree.files[rel] = true
		case info.IsDir():
			tree.dirs[rel] = true
			tree.unkeyable = append(tree.unkeyable, rel)
		default:
			// A socket, a pipe or a device has no bytes a key can hold.
			tree.unkeyable = append(tree.unkeyable, rel)
			continue
		}
		for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
			if tree.dirs[dir] {
				break
			}
			tree.dirs[dir] = true
		}
	}
	tree.paths = make([]string, 0, len(tree.files))
	for rel := range tree.files {
		tree.paths = append(tree.paths, rel)
	}
	sort.Strings(tree.paths)
	sort.Strings(tree.unkeyable)
	return tree, nil
}

// Root returns the directory the tree describes.
func (t *Tree) Root() string { return t.root }

// Paths returns every candidate that exists, regular file or symbolic link,
// as sorted slash paths relative to the root.
func (t *Tree) Paths() []string { return t.paths }

// Lstat reports what rel names without following a symbolic link at its last
// segment: a regular file, a symbolic link, or a directory that holds a
// candidate. A path through a symbolic link is not a candidate.
func (t *Tree) Lstat(rel string) (fs.FileMode, bool) {
	rel, ok := clean(rel)
	if !ok {
		return 0, false
	}
	if link, candidate := t.files[rel]; candidate {
		if link {
			return fs.ModeSymlink, true
		}
		return 0, true
	}
	if t.dirs[rel] {
		return fs.ModeDir, true
	}
	return 0, false
}

// Resolve follows the symbolic links on rel's way, from their target text,
// and returns the slash path it reaches relative to the root and what that
// path is. Only a candidate link is followed, and only to a candidate.
func (t *Tree) Resolve(rel string) (string, Kind) {
	current, ok := clean(rel)
	if !ok {
		return current, Outside
	}
	for hops := 0; ; hops++ {
		if current == "." {
			return current, Dir
		}
		segments := strings.Split(current, "/")
		prefix, followed := "", false
		for i, segment := range segments {
			prefix = path.Join(prefix, segment)
			link, candidate := t.files[prefix]
			if !candidate {
				if t.dirs[prefix] {
					continue
				}
				return current, Missing
			}
			if !link {
				if i < len(segments)-1 {
					// A file holds no entries.
					return current, Missing
				}
				return prefix, File
			}
			if hops >= maxLinkHops {
				return current, Missing
			}
			target, err := t.readlink(prefix)
			if err != nil {
				return current, Missing
			}
			target = filepath.ToSlash(target)
			if target == "" || path.IsAbs(target) || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
				return current, Outside
			}
			next, inside := clean(path.Join(append([]string{path.Dir(prefix), target}, segments[i+1:]...)...))
			if !inside {
				return next, Outside
			}
			current, followed = next, true
			break
		}
		if !followed {
			return current, Dir
		}
	}
}

// ReadFile returns the bytes of the regular candidate rel resolves to. It
// fails with an error matching fs.ErrNotExist when rel resolves to anything
// else.
func (t *Tree) ReadFile(rel string) ([]byte, error) {
	resolved, kind := t.Resolve(rel)
	if kind != File {
		return nil, &fs.PathError{Op: "read", Path: rel, Err: fs.ErrNotExist}
	}
	return os.ReadFile(filepath.Join(t.root, filepath.FromSlash(resolved))) //nolint:gosec // a candidate path inside the root
}

// Fingerprint digests the cut as a `git:` cache input reads it: each
// candidate's path, then its bytes and its source-v1 mode (regular or
// executable, read where a source binding reads it) or, for a symbolic link,
// its target text. Two cuts with one fingerprint hold the same paths, bytes,
// executable bits and link texts, so a `git:**` key of the root cannot tell
// them apart either. A test of a task that reads through a Tree uses it to
// prove the task's verdict moves only with what its key reads.
//
// It fails where the key produces none: on a candidate Git lists as a
// directory (a submodule's commit is not keyed), a socket, a pipe or a
// device, and on an unmerged index entry.
func (t *Tree) Fingerprint() (string, error) {
	if len(t.unkeyable) > 0 {
		return "", fmt.Errorf("git candidate %q is not a regular file or a symbolic link, so no `git:` key holds it", t.unkeyable[0])
	}
	index, err := readIndexModes(t.root)
	if err != nil {
		return "", err
	}
	if len(index.Unmerged) > 0 {
		unmerged := make([]string, 0, len(index.Unmerged))
		for rel := range index.Unmerged {
			unmerged = append(unmerged, rel)
		}
		sort.Strings(unmerged)
		return "", fmt.Errorf("git candidate %q has an unmerged index entry, so no `git:` key holds it", unmerged[0])
	}
	h := sha256.New()
	for _, rel := range t.paths {
		writeField(h, rel)
		if t.files[rel] {
			target, err := t.readlink(rel)
			if err != nil {
				return "", err
			}
			writeField(h, "symlink")
			writeField(h, target)
			continue
		}
		full := filepath.Join(t.root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(full) //nolint:gosec // a candidate path inside the root
		if err != nil {
			return "", err
		}
		writeField(h, "file")
		writeField(h, string(sourcebinding.FileMode(info.Mode(), index.Modes[rel], sourcebinding.HostStatsExecBit)))
		writeField(h, string(data))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeField writes one length-prefixed field, so no two field sequences
// share an encoding.
func writeField(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write(size[:])       //nolint:errcheck // hash.Hash.Write never errors
	h.Write([]byte(value)) //nolint:errcheck // hash.Hash.Write never errors
}

// readlink returns a candidate link's target text, read once.
func (t *Tree) readlink(rel string) (string, error) {
	if target, ok := t.links[rel]; ok {
		return target, nil
	}
	target, err := os.Readlink(filepath.Join(t.root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	t.links[rel] = target
	return target, nil
}

// clean returns rel as a clean slash path relative to the root, and whether it
// stays inside the root.
func clean(rel string) (string, bool) {
	rel = path.Clean(filepath.ToSlash(rel))
	if rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return rel, false
	}
	return rel, true
}
