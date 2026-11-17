package launch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The repositories the relay serves. A hook in the user's own hook file runs
// in every session of the person, whatever repository it opens, and the relay
// would start that repository's code: its putnamiw, or the CLI its lock pins
// with the extensions it loads by path. So the relay serves only a repository
// the person ran Putnami in, outside CI: every command that runs in a
// workspace records its repository here (CLI ADR 0098), and that command
// already ran the repository's code. A repository is named by its Git common
// directory, which every worktree of a clone shares, so a new worktree of a
// recorded clone is served at once.

// hostHookTrustRel is the record under the user's home.
const hostHookTrustRel = ".putnami/host-hooks/repositories.json"

// hostHookTrustMax bounds the record: the oldest entries go first.
const hostHookTrustMax = 1000

// hostHookTrustRecord is the document at hostHookTrustRel.
type hostHookTrustRecord struct {
	Version      int      `json:"version"`
	Repositories []string `json:"repositories"`
}

// HostHookTrustPath is the record of the repositories the relay serves, under
// home.
func HostHookTrustPath(home string) string {
	return filepath.Join(home, filepath.FromSlash(hostHookTrustRel))
}

// RecordHostHookRepository records the repository of the workspace at root,
// so the relay serves it. A repository already recorded leaves the record as
// it is, so a command that runs often reads it and writes nothing.
func RecordHostHookRepository(home, root string) error {
	identity := HostHookRepository(root)
	path := HostHookTrustPath(home)
	record, err := readHostHookTrust(path)
	if err != nil {
		return err
	}
	if slices.Contains(record.Repositories, identity) {
		return nil
	}
	record.Repositories = append(record.Repositories, identity)
	if extra := len(record.Repositories) - hostHookTrustMax; extra > 0 {
		record.Repositories = record.Repositories[extra:]
	}
	record.Version = 1
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".repositories-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// HostHookRepositoryRecorded reports whether the repository of the workspace
// at root is recorded under home. An unreadable record records nothing.
func HostHookRepositoryRecorded(home, root string) bool {
	record, err := readHostHookTrust(HostHookTrustPath(home))
	return err == nil && slices.Contains(record.Repositories, HostHookRepository(root))
}

func readHostHookTrust(path string) (hostHookTrustRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return hostHookTrustRecord{Version: 1}, nil
	}
	if err != nil {
		return hostHookTrustRecord{}, err
	}
	var record hostHookTrustRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Version != 1 {
		return hostHookTrustRecord{}, fmt.Errorf("%s is not a version 1 record of host hook repositories", path)
	}
	return record, nil
}

// HostHookRepository names the repository of the workspace at root: the Git
// common directory of its checkout, shared by every worktree of the clone, or
// the root itself outside Git. The name is absolute, with links resolved.
func HostHookRepository(root string) string {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotGit)
	switch {
	case err != nil:
		return canonicalPath(root)
	case info.IsDir():
		return canonicalPath(dotGit)
	case !info.Mode().IsRegular():
		return canonicalPath(root)
	}
	gitDir := readGitDirFile(dotGit)
	if gitDir == "" {
		return canonicalPath(root)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	// A worktree's Git directory points back at the worktree's .git file. A
	// .git file whose directory does not is no worktree of that clone: any
	// tree could name a recorded clone that way.
	if data, err := os.ReadFile(filepath.Join(gitDir, "gitdir")); err != nil ||
		canonicalPath(strings.TrimSpace(string(data))) != canonicalPath(dotGit) {
		return canonicalPath(root)
	}
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(data))
		if common != "" {
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitDir, common)
			}
			return canonicalPath(common)
		}
	}
	return canonicalPath(gitDir)
}

// readGitDirFile reads the `gitdir: <path>` line of a worktree's .git file.
func readGitDirFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "gitdir:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
