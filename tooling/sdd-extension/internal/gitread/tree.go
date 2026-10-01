package gitread

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"

	capabilityproto "go.putnami.dev/protocol/capabilities"
)

// ErrTreeObjectTooLarge identifies a bounded historical blob read that would
// exceed its caller's declared limit.
var ErrTreeObjectTooLarge = errors.New("git tree object exceeds read limit")

// TreeEntry is the exact Git metadata for one entry in an immutable tree.
type TreeEntry struct {
	Path     string
	Mode     string
	Type     string
	ObjectID string
}

// TreeEntryAt resolves one literal repository-relative path in commit. An
// absent entry returns (TreeEntry{}, false, nil).
func TreeEntryAt(repoRoot, commit, repoPath string) (TreeEntry, bool, error) {
	if !validObjectID(commit) {
		return TreeEntry{}, false, fmt.Errorf("inspect tree entry: invalid commit object ID")
	}
	if repoPath == "" {
		output, err := run(repoRoot, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
		if err != nil {
			return TreeEntry{}, false, fmt.Errorf("inspect repository tree: %w", err)
		}
		objectID := strings.ToLower(strings.TrimSpace(output))
		if !validObjectID(objectID) {
			return TreeEntry{}, false, fmt.Errorf("inspect repository tree: invalid tree object ID")
		}
		return TreeEntry{Mode: "040000", Type: "tree", ObjectID: objectID}, true, nil
	}
	if !validTreePath(repoPath) {
		return TreeEntry{}, false, fmt.Errorf("inspect tree entry: invalid repository path")
	}
	output, err := run(repoRoot, "ls-tree", "-z", "--full-name", commit, "--", ":(literal)"+repoPath)
	if err != nil {
		return TreeEntry{}, false, fmt.Errorf("inspect tree entry: %w", err)
	}
	entries := splitNUL(output)
	if len(entries) == 0 {
		return TreeEntry{}, false, nil
	}
	if len(entries) != 1 {
		return TreeEntry{}, false, fmt.Errorf("inspect tree entry: literal path returned multiple entries")
	}
	entry, err := parseTreeEntry(entries[0])
	if err != nil {
		return TreeEntry{}, false, err
	}
	if entry.Path != repoPath {
		return TreeEntry{}, false, fmt.Errorf("inspect tree entry: literal path did not round-trip")
	}
	return entry, true, nil
}

// TreeEntries lists the direct entries of one immutable tree object.
func TreeEntries(repoRoot, treeObjectID string) ([]TreeEntry, error) {
	if !validObjectID(treeObjectID) {
		return nil, fmt.Errorf("list tree entries: invalid tree object ID")
	}
	output, err := run(repoRoot, "ls-tree", "-z", treeObjectID)
	if err != nil {
		return nil, fmt.Errorf("list tree entries: %w", err)
	}
	items := splitNUL(output)
	entries := make([]TreeEntry, 0, len(items))
	for _, item := range items {
		entry, parseErr := parseTreeEntry(item)
		if parseErr != nil {
			return nil, parseErr
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// ReadTreeBlob returns one bounded blob's exact bytes without interpreting a
// revision:path expression supplied by a caller.
func ReadTreeBlob(repoRoot, objectID string, maximum int64) ([]byte, error) {
	if !validObjectID(objectID) || maximum < 0 {
		return nil, fmt.Errorf("read tree blob: invalid input")
	}
	typeOutput, err := run(repoRoot, "cat-file", "-t", objectID)
	if err != nil {
		return nil, fmt.Errorf("read tree blob type: %w", err)
	}
	if strings.TrimSpace(typeOutput) != "blob" {
		return nil, fmt.Errorf("read tree blob: object is not a blob")
	}
	sizeOutput, err := run(repoRoot, "cat-file", "-s", objectID)
	if err != nil {
		return nil, fmt.Errorf("read tree blob size: %w", err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeOutput), 10, 64)
	if err != nil || size < 0 {
		return nil, fmt.Errorf("read tree blob: invalid object size")
	}
	if size > maximum {
		return nil, ErrTreeObjectTooLarge
	}
	output, err := run(repoRoot, "cat-file", "blob", objectID)
	if err != nil {
		return nil, fmt.Errorf("read tree blob: %w", err)
	}
	if int64(len(output)) != size {
		return nil, fmt.Errorf("read tree blob: object size changed while reading")
	}
	return []byte(output), nil
}

// CommitSourceBinding computes source-v1 from one exact project root in an
// immutable commit. It uses Git modes and object bytes only; no index or
// worktree path participates.
func CommitSourceBinding(repoRoot, commit, projectRoot string) (string, error) {
	resolved, err := ResolveCommit(repoRoot, commit)
	if err != nil {
		return "", err
	}
	if projectRoot != "" && !validTreePath(projectRoot) {
		return "", fmt.Errorf("compute commit source binding: invalid project root")
	}
	rootEntry, exists, err := TreeEntryAt(repoRoot, resolved, projectRoot)
	if err != nil {
		return "", err
	}
	if !exists || rootEntry.Type != "tree" {
		return "", fmt.Errorf("compute commit source binding: project root is unavailable")
	}

	args := []string{"ls-tree", "-r", "-z", "--full-tree", resolved}
	if projectRoot != "" {
		args = append(args, "--", ":(literal)"+projectRoot)
	}
	output, err := run(repoRoot, args...)
	if err != nil {
		return "", fmt.Errorf("enumerate commit source: %w", err)
	}

	entries := splitNUL(output)
	records := make([]capabilityproto.SourceBindingFile, 0, len(entries))
	blobs := make([]string, 0, len(entries))
	for _, item := range entries {
		entry, parseErr := parseTreeEntry(item)
		if parseErr != nil {
			return "", parseErr
		}
		relative := entry.Path
		if projectRoot != "" {
			prefix := projectRoot + "/"
			if !strings.HasPrefix(entry.Path, prefix) {
				return "", fmt.Errorf("enumerate commit source: entry escapes project root")
			}
			relative = strings.TrimPrefix(entry.Path, prefix)
		}
		record := capabilityproto.SourceBindingFile{Path: relative, Mode: capabilityproto.SourceFileMode(entry.Mode)}
		switch entry.Type {
		case "blob":
			blobs = append(blobs, entry.ObjectID)
			record.Digest = entry.ObjectID
		case "commit":
			if entry.Mode != string(capabilityproto.SourceModeGitlink) {
				return "", fmt.Errorf("enumerate commit source: commit entry has unsupported mode")
			}
			record.Digest = "git:" + strings.ToLower(entry.ObjectID)
		default:
			return "", fmt.Errorf("enumerate commit source: unsupported entry type")
		}
		records = append(records, record)
	}

	digests, err := treeBlobDigests(repoRoot, blobs)
	if err != nil {
		return "", err
	}
	for index := range records {
		if strings.HasPrefix(records[index].Digest, "git:") {
			continue
		}
		digest, ok := digests[records[index].Digest]
		if !ok {
			return "", fmt.Errorf("compute commit source binding: blob digest unavailable")
		}
		records[index].Digest = digest
	}
	binding, err := capabilityproto.ComputeSourceBinding(records)
	if err != nil {
		return "", fmt.Errorf("compute commit source binding: %w", err)
	}
	return binding, nil
}

func parseTreeEntry(value string) (TreeEntry, error) {
	metadata, entryPath, found := strings.Cut(value, "\t")
	if !found {
		return TreeEntry{}, fmt.Errorf("parse tree entry: missing path separator")
	}
	fields := strings.Fields(metadata)
	if len(fields) != 3 || !validObjectID(fields[2]) || entryPath == "" {
		return TreeEntry{}, fmt.Errorf("parse tree entry: malformed metadata")
	}
	return TreeEntry{Mode: fields[0], Type: fields[1], ObjectID: strings.ToLower(fields[2]), Path: entryPath}, nil
}

func validTreePath(value string) bool {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func treeBlobDigests(repoRoot string, objectIDs []string) (map[string]string, error) {
	unique := make(map[string]bool, len(objectIDs))
	ids := make([]string, 0, len(objectIDs))
	for _, objectID := range objectIDs {
		if !validObjectID(objectID) {
			return nil, fmt.Errorf("hash tree blobs: invalid object ID")
		}
		if !unique[objectID] {
			unique[objectID] = true
			ids = append(ids, objectID)
		}
	}
	sort.Strings(ids)
	digests := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return digests, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir = repoRoot
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("hash tree blobs: open git output: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("hash tree blobs: start git: %w", err)
	}
	waited := false
	defer func() {
		if waited || cmd.Process == nil {
			return
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	for _, expected := range ids {
		header, readErr := reader.ReadString('\n')
		if readErr != nil {
			return nil, fmt.Errorf("hash tree blobs: read object header: %w", readErr)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || strings.ToLower(fields[0]) != expected || fields[1] != "blob" {
			return nil, fmt.Errorf("hash tree blobs: malformed batch header")
		}
		size, parseErr := strconv.ParseInt(fields[2], 10, 64)
		if parseErr != nil || size < 0 {
			return nil, fmt.Errorf("hash tree blobs: invalid blob size")
		}
		hash := sha256.New()
		if _, copyErr := io.CopyN(hash, reader, size); copyErr != nil {
			return nil, fmt.Errorf("hash tree blobs: read blob body: %w", copyErr)
		}
		separator, readErr := reader.ReadByte()
		if readErr != nil || separator != '\n' {
			return nil, fmt.Errorf("hash tree blobs: malformed blob separator")
		}
		digests[expected] = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("hash tree blobs: timed out after %s", gitTimeout)
		}
		return nil, fmt.Errorf("hash tree blobs: git failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return digests, nil
}
