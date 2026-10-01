package git

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/sourcebinding"
)

// TreeFingerprint identifies one worktree's exact state: the commit it sits on,
// the bytes at every tracked path that differs from that commit — recursively,
// through submodules — and the content of every untracked, non-ignored file.
//
// It exists because "the same files are dirty" is not "the same bytes are on
// disk". `git status --porcelain` reports paths and status codes, so an agent
// that gates a tree and then edits one of the files it had already dirtied
// leaves the status output byte-for-byte identical: a path-only check accepts a
// gate that proved nothing about the content now on disk. Comparing this digest
// does prove it.
//
// HEAD is folded in as well as the delta. A delta is only meaningful against the
// commit it was taken from, so folding HEAD in means a moved HEAD shows up as a
// different tree instead of canceling out against a coincidentally identical
// set of changes.
//
// This is the ONE implementation. `putnami tree fingerprint` prints it and
// recorded sessions carry it (protocol cli.SessionTree); the fix skill's
// tree-fingerprint.sh is a thin caller of that command rather than a second copy
// of the calculation, because two agents comparing fingerprints computed by two
// implementations compare nothing.
type TreeFingerprint struct {
	// Fingerprint is the lowercase hex sha256 of the canonical stream below.
	Fingerprint string
	// Dirty reports whether the worktree differed from HeadSHA. It is derived
	// from the very entries the digest hashed — at least one changed tracked
	// path or one untracked entry — rather than from a second inspection, so the
	// two answers can never disagree about the same tree.
	Dirty bool
	// HeadSHA is the full object id HEAD resolved to.
	HeadSHA string
}

// treeFingerprintPreimageVersion opens the canonical stream. It is part of the
// digest, so a future change to the encoding cannot be mistaken for a change to
// the tree: every fingerprint moves at once and loudly.
//
// Version 2 replaced version 1's rendered `git diff` output. That output was
// BOTH configuration-dependent and lossy, and each half broke the digest's one
// promise:
//
//   - `core.abbrev`, `diff.mnemonicPrefix`, `diff.noprefix` and `diff.srcPrefix`
//     each changed the digest of an unchanged tree. `--binary` does not imply
//     `--full-index` for a text diff, so the `index 83db..ed51` line abbreviated
//     to whatever the reader's configuration asked for. Two machines could
//     disagree about the same bytes.
//   - A dirty submodule rendered as `Subproject commit <sha>-dirty`, whatever
//     was actually inside it. Editing a file in an already-dirty submodule left
//     the parent's digest identical — the exact "same status, different bytes"
//     defect this whole mechanism exists to catch, one level down.
//
// Version 2 hashes structure instead: which paths differ from HEAD, and the real
// bytes at each of them. Nothing in the preimage is rendered by git.
const treeFingerprintPreimageVersion = "putnami-tree-fingerprint/2"

// Entry kinds. They are in the preimage because a chmod +x or a file-to-symlink
// swap changes what the tree DOES while leaving the bytes it contains identical.
const (
	treeEntryFile      = "file"
	treeEntryExec      = "exec"
	treeEntryLink      = "link"
	treeEntryDirectory = "dir"
	// treeEntryGone is a tracked path that is no longer in the worktree. It has
	// no content to hash, and it is its own kind so that "deleted" cannot be
	// confused with "present and empty".
	treeEntryGone = "gone"
	// treeEntrySubmodule is a tracked gitlink whose own worktree was
	// fingerprinted; its digest is that recursive fingerprint.
	treeEntrySubmodule = "sub"
	// treeEntrySubmoduleAbsent is a gitlink with no checked-out worktree — an
	// uninitialized submodule. There is no content on disk to hash, so the
	// recorded digest covers the gitlink object id instead, which is all the
	// tree actually contains.
	treeEntrySubmoduleAbsent = "sub-absent"
)

// git's file modes as `status --porcelain=v2` spells them, six octal digits.
const (
	// gitlinkMode is a submodule entry.
	gitlinkMode = "160000"
	// absentMode is what git reports for a path that is not in the worktree.
	// It is "000000", not "0": the field is fixed-width, and comparing against
	// the short spelling silently classified every deleted file as a regular one
	// and then failed to stat it.
	absentMode = "000000"
	// executableMode is an executable regular file.
	executableMode = "100755"
)

// hostStatsExecBit reports whether the host filesystem stores the executable
// bit. Windows does not: every file stats without one there, so a tracked file
// takes its executable bit from the mode git reports for it instead. The
// source binding reads the same constant.
const hostStatsExecBit = sourcebinding.HostStatsExecBit

// maxSubmoduleDepth bounds the recursion. Submodules nest legitimately, but a
// malformed superproject must fail loudly rather than recurse until the stack
// gives out.
const maxSubmoduleDepth = 10

// FingerprintTree computes the fingerprint of the worktree containing dir.
//
// It reads the repository and writes nothing: no session record, no workspace
// state, no file. dir may be any directory inside the worktree — the repository
// root is resolved here, so two callers in different directories agree.
//
// The canonical preimage, which doc/02-result-v2.md § Gated tree fingerprint
// states for non-Go consumers:
//
//	putnami-tree-fingerprint/2\n
//	head <headSHA>\n
//	tracked <count>\n
//	<entry>   (× count, ordered by path)
//	untracked <count>\n
//	<entry>   (× count, ordered by path)
//
// where each <entry> is:
//
//	<kind> <sha256 of the entry's content> <byte length of path>\n<path>\n
//
// The digest is sha256 of that stream. Every variable-length part is either
// already a fixed-width digest or carries its byte length, so no path can be
// confused with another one plus a separator — a fingerprint that can be forged
// by naming a file cleverly is not a fingerprint.
//
// The tracked section lists exactly the paths that differ from HEAD, and hashes
// the bytes ON DISK at each of them. It never hashes git's rendering of that
// difference: see treeFingerprintPreimageVersion for the two defects that cost.
func FingerprintTree(dir string) (TreeFingerprint, error) {
	return fingerprintTree(dir, 0)
}

func fingerprintTree(dir string, depth int) (TreeFingerprint, error) {
	if depth > maxSubmoduleDepth {
		return TreeFingerprint{}, fmt.Errorf("submodules nest more than %d deep at %s", maxSubmoduleDepth, dir)
	}
	root, err := worktreeRoot(dir)
	if err != nil {
		return TreeFingerprint{}, err
	}
	head, err := run(root, "rev-parse", "HEAD")
	if err != nil {
		return TreeFingerprint{}, fmt.Errorf("the repository has no commit, so there is no tree to fingerprint")
	}
	head = strings.TrimSpace(head)

	tracked, err := trackedChanges(root, depth, hostStatsExecBit)
	if err != nil {
		return TreeFingerprint{}, err
	}

	// `git ls-files --others` emits its entries in sorted order, so the untracked
	// section is stable across machines without a sort of its own.
	listing, err := run(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return TreeFingerprint{}, fmt.Errorf("list untracked files: %w", err)
	}
	untracked := splitNUL(listing)

	digest := sha256.New()
	fmt.Fprintf(digest, "%s\n", treeFingerprintPreimageVersion)
	fmt.Fprintf(digest, "head %s\n", head)
	fmt.Fprintf(digest, "tracked %d\n", len(tracked))
	for _, entry := range tracked {
		fmt.Fprintf(digest, "%s %s %d\n%s\n", entry.kind, entry.content, len(entry.path), entry.path)
	}
	fmt.Fprintf(digest, "untracked %d\n", len(untracked))
	for _, path := range untracked {
		kind, content, err := hashUntracked(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return TreeFingerprint{}, err
		}
		fmt.Fprintf(digest, "%s %s %d\n%s\n", kind, content, len(path), path)
	}

	return TreeFingerprint{
		Fingerprint: hex.EncodeToString(digest.Sum(nil)),
		Dirty:       len(tracked) > 0 || len(untracked) > 0,
		HeadSHA:     head,
	}, nil
}

// treeEntry is one path the digest covers, with the digest of what is at it.
type treeEntry struct {
	kind    string
	content string
	path    string
}

// trackedChanges lists the tracked paths whose worktree state differs from HEAD,
// each with the digest of what is actually on disk there.
//
// It reads `git status --porcelain=v2`, which is a documented machine format
// carrying object ids and modes as fields rather than as rendered text. The
// flags pin the three status settings a repository could otherwise change under
// us: renames are off (a rename is just a delete and an add here, and rename
// detection is score-based and configurable), untracked entries are excluded
// because they are listed separately and in sorted order, and submodules are
// never ignored — `diff.ignoreSubmodules` or `submodule.<name>.ignore` would
// otherwise hide a dirty submodule from the digest entirely.
//
// `-z` also removes `core.quotePath` from the picture: NUL-terminated output is
// never quoted or escaped.
//
// Not covered, because git itself does not report it: a path marked
// `assume-unchanged` or `skip-worktree` is one the repository has asked git to
// stop looking at, and `core.fileMode=false` tells git to ignore the executable
// bit. Those are repository-wide settings that every reader of that repository
// shares, not per-machine rendering.
//
// statsExecBit says whether a regular file's executable kind comes from its
// permission bits (true) or from the worktree mode git reports (false); see
// trackedKind.
func trackedChanges(root string, depth int, statsExecBit bool) ([]treeEntry, error) {
	listing, err := run(root, "status", "--porcelain=v2", "-z", "--no-renames", "--ignore-submodules=none", "-uno")
	if err != nil {
		return nil, fmt.Errorf("read the tracked changes: %w", err)
	}

	records := splitNUL(listing)
	entries := make([]treeEntry, 0, len(records))
	for index := 0; index < len(records); index++ {
		record := records[index]
		if record == "" {
			continue
		}
		switch record[0] {
		case '1', '2':
			// "1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>", and for a rename
			// "2 ... <X><score> <path>" followed by the original path in its own
			// NUL-terminated field. --no-renames should rule type 2 out; consuming
			// the extra field anyway keeps the scan aligned if git reports one.
			fields := strings.SplitN(record, " ", 9)
			if len(fields) < 9 {
				return nil, fmt.Errorf("git status emitted an entry with %d fields, want 9: %q", len(fields), record)
			}
			path := fields[8]
			if record[0] == '2' {
				path = strings.SplitN(fields[8], " ", 2)[1]
				index++ // the original path
			}
			entry, err := trackedEntry(root, path, fields[5], fields[7], depth, statsExecBit)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		case 'u':
			// "u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>". An
			// unmerged path has conflict markers on disk, and those bytes are the
			// tree's state as much as any other.
			fields := strings.SplitN(record, " ", 11)
			if len(fields) < 11 {
				return nil, fmt.Errorf("git status emitted an unmerged entry with %d fields, want 11: %q", len(fields), record)
			}
			entry, err := trackedEntry(root, fields[10], fields[6], fields[7], depth, statsExecBit)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		case '?', '!':
			// Listed separately, or deliberately outside the tree a gate reasons
			// about. `-uno` should exclude both.
			continue
		default:
			return nil, fmt.Errorf("git status emitted an unrecognized record: %q", record)
		}
	}

	// git orders status by path already, but the contract states the order, so it
	// is enforced here rather than inherited.
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

// trackedEntry digests one changed path, given the worktree mode and the index
// object id git reported for it.
func trackedEntry(root, path, worktreeMode, objectID string, depth int, statsExecBit bool) (treeEntry, error) {
	absolute := filepath.Join(root, filepath.FromSlash(path))
	switch {
	case worktreeMode == absentMode:
		// The path is gone from the worktree. There is nothing to read, and the
		// kind says so rather than letting a deletion hash like an empty file.
		return treeEntry{kind: treeEntryGone, content: hashBytes(nil), path: path}, nil
	case worktreeMode == gitlinkMode:
		// A submodule's content is the whole point of this case. Hashing the
		// gitlink alone is what version 1 effectively did through
		// `Subproject commit <sha>-dirty`, and it let an edit inside an
		// already-dirty submodule pass unnoticed.
		//
		// The checkout test comes FIRST and is not a fallback from a failed
		// recursion: an uninitialized submodule is an empty directory inside the
		// parent's worktree, so `rev-parse --show-toplevel` inside it answers with
		// the PARENT's root. Recursing on that would fingerprint the parent from
		// within itself, once per level, until the depth guard fired.
		if _, err := os.Stat(filepath.Join(absolute, ".git")); err != nil {
			// Not checked out. Its content is genuinely not on disk, so the
			// gitlink object id is all this tree holds for it.
			return treeEntry{kind: treeEntrySubmoduleAbsent, content: hashBytes([]byte(objectID)), path: path}, nil
		}
		nested, err := fingerprintTree(absolute, depth+1)
		if err != nil {
			return treeEntry{}, fmt.Errorf("fingerprint submodule %s: %w", path, err)
		}
		return treeEntry{kind: treeEntrySubmodule, content: hashBytes([]byte(nested.Fingerprint)), path: path}, nil
	default:
		kind, content, err := hashUntracked(absolute)
		if err != nil {
			return treeEntry{}, err
		}
		return treeEntry{kind: trackedKind(kind, worktreeMode, statsExecBit), content: content, path: path}, nil
	}
}

// trackedKind is the kind of a tracked path whose disk entry classified as
// diskKind. When the host stats the executable bit, diskKind stands. Otherwise a
// regular file is executable exactly when worktreeMode, the mode git reports
// for it, is executableMode; links and directories keep diskKind.
func trackedKind(diskKind, worktreeMode string, statsExecBit bool) string {
	if statsExecBit || (diskKind != treeEntryFile && diskKind != treeEntryExec) {
		return diskKind
	}
	if worktreeMode == executableMode {
		return treeEntryExec
	}
	return treeEntryFile
}

// worktreeRoot resolves the repository root holding dir. It is the one place
// that decides which tree is being fingerprinted, so a caller's working
// directory never leaks into the digest.
func worktreeRoot(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	output, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git worktree", dir)
	}
	root := strings.TrimSpace(output)
	if root == "" {
		return "", fmt.Errorf("%s is not inside a git worktree", dir)
	}
	return root, nil
}

// hashUntracked classifies one untracked entry and digests what it holds.
//
// A symlink hashes its TARGET rather than what the target points at: following
// it would make a dangling link unreadable and would fold a file that is already
// hashed under its own path into a second entry.
//
// A directory entry is git's own spelling for a nested repository (`ls-files
// --others` collapses one to `<path>/`), and git refuses to hash it. Its path
// still enters the digest; its contents do not, because they are not part of
// THIS repository's tree state as git reports it — `git status` says nothing
// about them either. filepath.Join has already cleaned the trailing separator
// off the filesystem path, while the digest still records git's spelling — so
// `foo` and `foo/` stay distinct entries.
func hashUntracked(path string) (kind string, content string, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		// The tree is moving under us. Say so rather than invent a digest: a
		// fingerprint of a tree that no longer exists would be compared as if it
		// described one that does.
		return "", "", fmt.Errorf("fingerprint untracked entry: %w", err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", "", fmt.Errorf("fingerprint untracked symlink: %w", err)
		}
		return treeEntryLink, hashBytes([]byte(target)), nil
	case info.IsDir():
		return treeEntryDirectory, hashBytes(nil), nil
	}

	file, err := os.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("fingerprint untracked file: %w", err)
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, copyErr := io.Copy(hasher, file); copyErr != nil {
		return "", "", fmt.Errorf("fingerprint untracked file: %w", copyErr)
	}
	kind = treeEntryFile
	if info.Mode().Perm()&0o111 != 0 {
		kind = treeEntryExec
	}
	return kind, hex.EncodeToString(hasher.Sum(nil)), nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
