package artifactstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The GC's live set. A workspace uses a store entry through a symlink under its
// .putnami/bin: bin/putnami for the CLI, bin/<kind>/<name> for an extension,
// template or agent artifact. Recency alone cannot say whether a workspace still
// needs an entry — a workspace left alone past MaxIdle, or one whose pins are
// older than a burst of fresh from-source builds, would lose the very binaries
// it runs. So every workspace the CLI starts in registers itself under roots/,
// and GC never evicts an entry a registered workspace links to, whatever its age.
//
//	<root>/roots/<id> → <workspace root>     one symlink per workspace
//
// A root whose workspace no longer exists is pruned by the next GC; the entries
// it alone kept alive then age out under the ordinary idle and budget rules.
const (
	rootsDirName = "roots"
	// rootIDLen is the hex length of a root's name: 128 bits of the workspace
	// path's SHA-256, far past any collision risk for one machine's workspaces.
	rootIDLen = 32
)

// RegisterWorkspace records wsRoot as a GC root, so the entries its .putnami/bin
// links to are never evicted while it exists. It is idempotent and cheap on the
// warm path (one readlink); a stale or missing root is replaced atomically.
func (s *Store) RegisterWorkspace(wsRoot string) error {
	if wsRoot == "" {
		return nil
	}
	abs, err := filepath.Abs(wsRoot)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.root, rootsDirName)
	link := filepath.Join(dir, rootID(abs))
	if target, err := os.Readlink(link); err == nil && target == abs {
		return nil
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	// Same atomic swap as the workspace links: a process-unique temp name, then
	// rename, so concurrent registrations of one workspace never interleave.
	tmp := fmt.Sprintf("%s.tmp.%d", link, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(abs, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// TouchExecutable stamps the recency of the store entry holding path — the
// running CLI's own binary — and is a no-op for a path outside the store. A
// from-source session and its nested invocations exec the blob directly, so
// without this a gate running longer than the grace window could see a
// concurrent GC evict the binary its next child needs.
func (s *Store) TouchExecutable(path string) {
	if path == "" {
		return
	}
	if dir, ok := s.entryFor(path); ok {
		if _, err := os.Stat(dir); err == nil {
			stampUsed(dir, time.Now())
		}
	}
}

// liveEntries returns the entry dirs that a registered workspace links to, and
// prunes the roots whose workspace is gone. Callers hold the exclusive lock.
func (s *Store) liveEntries() map[string]bool {
	live := map[string]bool{}
	dir := filepath.Join(s.root, rootsDirName)
	roots, err := os.ReadDir(dir)
	if err != nil {
		return live
	}
	for _, r := range roots {
		link := filepath.Join(dir, r.Name())
		ws, err := os.Readlink(link)
		if err != nil {
			continue // not a root this package wrote; leave it alone
		}
		if _, err := os.Lstat(ws); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(link)
			continue
		}
		s.collectLinks(filepath.Join(ws, ".putnami", "bin"), live)
	}
	return live
}

// collectLinks marks the store entries that the symlinks directly under bin, or
// one directory below it, resolve to: bin/putnami and bin/<kind>/<name>.
func (s *Store) collectLinks(bin string, live map[string]bool) {
	entries, err := os.ReadDir(bin)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(bin, e.Name())
		if e.Type()&fs.ModeSymlink != 0 {
			s.markLink(p, live)
			continue
		}
		if !e.IsDir() {
			continue
		}
		children, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, c := range children {
			if c.Type()&fs.ModeSymlink != 0 {
				s.markLink(filepath.Join(p, c.Name()), live)
			}
		}
	}
}

func (s *Store) markLink(link string, live map[string]bool) {
	target, err := os.Readlink(link)
	if err != nil {
		return
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	if dir, ok := s.entryFor(target); ok {
		live[dir] = true
	}
}

// entryFor maps a path inside the store to the entry dir GC evicts as a unit:
// sha256/<xx>/<digest>, cli/<sha> or cli-source/<key>. The dir is spelled from
// s.root, exactly as GC enumerates it, so a link written through a symlinked
// spelling of the root (macOS /var → /private/var) still matches.
func (s *Store) entryFor(path string) (string, bool) {
	path = filepath.Clean(path)
	bases := []string{s.root}
	if real, err := filepath.EvalSymlinks(s.root); err == nil && real != s.root {
		bases = append(bases, real)
	}
	for _, base := range bases {
		rel, err := filepath.Rel(base, path)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case parts[0] == shaDirName && len(parts) >= 3:
			return filepath.Join(s.root, parts[0], parts[1], parts[2]), true
		case (parts[0] == cliDirName || parts[0] == cliSourceDirName) && len(parts) >= 2:
			return filepath.Join(s.root, parts[0], parts[1]), true
		}
	}
	return "", false
}

func rootID(wsRoot string) string {
	sum := sha256.Sum256([]byte(wsRoot))
	return hex.EncodeToString(sum[:])[:rootIDLen]
}
