package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

// coreInstallStateInputs are the workspace-root files CORE itself owns for the
// first-use install decision. There is exactly one: the putnami lock, which
// captures the installed extension set and their versions.
//
// Everything else that used to be here — bun.lock, bun.lockb, uv.lock, go.work —
// was a hard-coded list of language filenames inside a language-neutral
// orchestrator, and a later cleanup deleted it. Those files are now
// named by the extensions that install from them, through the `workspace.inputs`
// their adapters declare, and reach this decision through
// workspaceInstallStateInputs.
var coreInstallStateInputs = []string{"putnami.lock.json"}

// installStateInputShapes is the LOCK-SHAPED filter applied to what the adapters
// declare, and it is the narrow answer to a code-review finding.
//
// An adapter's `workspace.inputs` answers ONE question: "what decides this
// provider's probe answer?". The first-use install marker asks a different one:
// "is the installed tree still the tree the lock describes?". That cutover fed
// the first set to the second, which conscripted every root-level probe input —
// so editing the repo-root `biome.json` or `tsconfig.base.json` made the next
// command run a full workspace re-install. Those files change what a project
// RESOLVES to; they install nothing.
//
// The correct separation is a declaration, not a filter: an adapter naming its
// own install inputs. That needs a `workspace.installInputs` field on the
// extension manifest — a protocol change with a schema bump and a twin on both
// runtimes — so until it exists, core keeps this list, which is exactly the
// pre-epic set restored as an intersection rather than as a source. The
// difference from the deleted table matters: core does not go looking for these
// files, it only ACCEPTS them when the owning adapter has declared them, so a
// workspace with no Python extension still has no uv.lock in its fingerprint.
//
// go.work is a lock in the sense that counts here: it names the modules the
// workspace resolves against, and adding one changes what an install must
// prepare. Go's checksum files stay out, because ordinary build and deploy flows
// update them and a bootstrap input a normal command invalidates would re-run
// the install on every other command.
var installStateInputShapes = map[string]bool{
	"putnami.lock.json": true,
	"bun.lock":          true,
	"bun.lockb":         true,
	"uv.lock":           true,
	"go.work":           true,
}

// workspaceInstallStateInputs is the workspace-ROOT file set whose content
// decides whether the installed workspace state is current: core's own lock plus
// every root-level, LOCK-SHAPED input the workspace adapters declare.
//
// The declarations are read from the persisted workspace index rather than by
// discovering extensions, and that is a cost decision as much as a layering one.
// EnsureWorkspaceBootstrap runs on EVERY command; its steady-state path is a
// handful of stats, and paying extension discovery there would turn a ~0.015 ms
// check into the most expensive thing a no-op command does. The index already
// records what each adapter declared, with digests, so reading it is one file.
//
// A workspace with no index yet — a fresh checkout — resolves to core's lock
// alone, which is the correct answer for the case the marker exists to catch:
// nothing has been installed, so the install must run. The same holds if the
// index is DELETED under a workspace that was installed: the recorded marker
// then names inputs this function can no longer justify, the fingerprints
// disagree, and the install re-runs. Erring toward re-installing is the right
// direction for a decision that exists to make a fresh checkout work.
//
// Only ROOT-level inputs count. A per-project manifest is a metadata input, not
// an install input; it invalidates the workspace probe, not the installed tree.
func workspaceInstallStateInputs(wsRoot string) []string {
	names := make([]string, 0, len(coreInstallStateInputs)+4)
	names = append(names, coreInstallStateInputs...)

	snapshot, err := workspace.LoadSnapshot(wsRoot)
	if err == nil && snapshot != nil {
		for _, provider := range snapshot.Providers {
			for _, input := range provider.Inputs {
				if strings.Contains(input.Path, "/") || strings.ContainsAny(input.Path, "*?[") {
					continue
				}
				if !installStateInputShapes[input.Path] {
					continue
				}
				names = append(names, input.Path)
			}
		}
	}

	sort.Strings(names)
	deduped := names[:0]
	previous := ""
	for i, name := range names {
		if i > 0 && name == previous {
			continue
		}
		previous = name
		deduped = append(deduped, name)
	}
	return deduped
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

// scanLockFiles stats every candidate install input at the workspace root and
// hashes the content of those present, in sorted name order (so the result is
// deterministic). Absent files are skipped, which is what keeps the set
// language-agnostic: a workspace with no Python extension simply has no uv.lock
// to record.
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
// is called at the end of a successful `putnami install`, so the recorded
// fingerprint reflects post-install lock files (e.g. a bun.lock that
// `bun install` just regenerated).
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
