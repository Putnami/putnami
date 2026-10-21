package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// installStateFilename is the workspace-local sidecar that records the state of
// the last successful `putnami install`. It lives under .putnami/ (gitignored),
// so a fresh checkout or new worktree starts without it and is treated as
// needing a first-use install. See EnsureWorkspaceBootstrap.
const installStateFilename = "install-state.json"

// installStateVersion is the marker schema version. A mismatch is treated as
// stale so an old marker triggers a re-install rather than being misread.
const installStateVersion = 1

// coreInstallStateInput is the one workspace-root file the core itself owns
// for the first-use install decision: the putnami lock, which records the
// installed extensions and their versions. Every other input is a file an
// extension declares in the installInputs of its workspace adapter.
const coreInstallStateInput = "putnami.lock.json"

// workspaceInstallStateInputs is the sorted set of workspace-root file names
// whose content decides whether the installed workspace state is current: the
// core's lock plus the install inputs of every provider the workspace index
// records. A provider that declares no install inputs adds nothing, so the
// content of its lockfiles does not decide the install state.
//
// The declarations are read from the persisted workspace index, not from the
// extension manifests. EnsureWorkspaceBootstrap runs on every command, and its
// steady-state path is a handful of stats; extension discovery would make it
// the most expensive step of a command that does nothing. The index records
// what each adapter declared, so reading it is one file.
//
// The bootstrap reads the index before the command's first workspace sync, so
// a changed declaration counts from the first command after a sync records it.
// An index the core cannot read, or a workspace with no index yet, resolves to
// the core's lock alone; a marker that recorded more files then disagrees, and
// the install runs.
//
// A recorded name the protocol refuses as an install input is skipped, so a
// hand-edited index never makes the check read a file outside the workspace
// root.
func workspaceInstallStateInputs(wsRoot string) []string {
	names := []string{coreInstallStateInput}

	snapshot, err := workspace.LoadSnapshot(wsRoot)
	if err == nil && snapshot != nil {
		for _, provider := range snapshot.Providers {
			for _, name := range provider.InstallInputs {
				if clean, err := extproto.NormalizeInstallInput(name); err != nil || clean != name {
					continue
				}
				names = append(names, name)
			}
		}
	}

	slices.Sort(names)
	return slices.Compact(names)
}

// lockFingerprint records a single lock file's identity. Size+ModTime drive the
// stat-only fast path; Hash is the content oracle consulted only when size or
// mtime moved (so a git pull that touches mtimes without changing content does
// not trigger a spurious re-install).
type lockFingerprint struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtimeNs"`
	Hash    string `json:"hash"`
}

// InstallState is the sidecar marker contents.
type InstallState struct {
	Version     int               `json:"version"`
	InstalledAt string            `json:"installedAt"`
	Fingerprint string            `json:"fingerprint"`
	LockFiles   []lockFingerprint `json:"lockFiles"`
}

func installStatePath(wsRoot string) string {
	return filepath.Join(wsRoot, ".putnami", installStateFilename)
}

// hashFile returns the hex sha256 of a file's content, streaming so the size of
// the lock file does not matter.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// scanLockFiles stats every install input at the workspace root and hashes the
// content of those present, in sorted name order (so the result is
// deterministic). An absent input is not recorded, so a declared file that
// appears or disappears later changes the recorded set.
func scanLockFiles(wsRoot string) ([]lockFingerprint, error) {
	var out []lockFingerprint
	for _, name := range workspaceInstallStateInputs(wsRoot) {
		p := filepath.Join(wsRoot, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		hash, err := hashFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, lockFingerprint{
			Path:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
			Hash:    hash,
		})
	}
	return out, nil
}

// combinedFingerprint folds the per-file hashes into one digest. Order is fixed
// by scanLockFiles, so the same content always yields the same fingerprint.
func combinedFingerprint(files []lockFingerprint) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\x00", f.Path, f.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readInstallState reads the sidecar marker. A missing marker returns (nil, nil)
// — that is the fresh-checkout signal, not an error.
func readInstallState(wsRoot string) (*InstallState, error) {
	data, err := os.ReadFile(installStatePath(wsRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st InstallState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// writeInstallStateRaw atomically writes the given marker (create temp + rename,
// never rewrite in place — matching the layout package's symlink discipline).
func writeInstallStateRaw(wsRoot string, st *InstallState) error {
	path := installStatePath(wsRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeInstallState captures the current workspace lock state as the marker. It
// is called at the end of a successful `putnami install`, after the install's
// own workspace sync, so the marker records the install inputs the index
// declares then, with the content the installers left.
func writeInstallState(wsRoot string) error {
	files, err := scanLockFiles(wsRoot)
	if err != nil {
		return err
	}
	return writeInstallStateRaw(wsRoot, &InstallState{
		Version:     installStateVersion,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
		Fingerprint: combinedFingerprint(files),
		LockFiles:   files,
	})
}

// statFastMatch reports whether every recorded lock file is still present with
// the same size and mtime, and no new lock file appeared. This is the ~0.015ms
// steady-state path: pure stat, no reads, no hashing.
func statFastMatch(st *InstallState, wsRoot string) bool {
	recorded := make(map[string]lockFingerprint, len(st.LockFiles))
	for _, f := range st.LockFiles {
		recorded[f.Path] = f
	}
	present := 0
	for _, name := range workspaceInstallStateInputs(wsRoot) {
		info, err := os.Stat(filepath.Join(wsRoot, name))
		if err != nil || info.IsDir() {
			continue
		}
		present++
		r, had := recorded[name]
		if !had || r.Size != info.Size() || r.ModTime != info.ModTime().UnixNano() {
			return false
		}
	}
	return present == len(st.LockFiles)
}

// evalInstall decides whether the workspace install is current.
//
//	stale == true            -> a (re)install is needed: the marker is missing,
//	                            its schema version differs, the lock-file set
//	                            changed, or lock content actually changed.
//	stale == false, repair != nil -> current, but lock-file mtimes moved while
//	                            content stayed identical (e.g. a git pull). The
//	                            returned marker has refreshed size/mtime (and the
//	                            original InstalledAt/Fingerprint) so the caller can
//	                            rewrite it and restore the stat-only fast path.
//	stale == false, repair == nil -> current, nothing to do.
//
// On a hosted run (runcredential.Hosted) every install is stale. The marker
// lives in the workspace, so the repository can commit one that matches its
// locks. A current marker would skip the hosted install and, with it, the
// workspace-fetch that downloads the dependencies before any repository code,
// and the remote cache provider start that follows it.
func evalInstall(wsRoot string) (stale bool, repair *InstallState) {
	if runcredential.Hosted() {
		return true, nil
	}
	st, err := readInstallState(wsRoot)
	if err != nil || st == nil || st.Version != installStateVersion {
		return true, nil
	}
	if statFastMatch(st, wsRoot) {
		return false, nil
	}
	// Size/mtime moved or the set changed — confirm against content so a pure
	// mtime bump does not force a re-install.
	current, err := scanLockFiles(wsRoot)
	if err != nil {
		return true, nil
	}
	if combinedFingerprint(current) != st.Fingerprint {
		return true, nil
	}
	// Identical content, only mtimes moved: refresh the recorded stat metadata
	// but keep InstalledAt and Fingerprint.
	st.LockFiles = current
	return false, st
}
