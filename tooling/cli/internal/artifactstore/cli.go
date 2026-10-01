package artifactstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"go.putnami.dev/sdk/extension/pkgmeta"
)

// cliBinaryName is the executable filename inside a cli/<sha>/ entry on this
// machine. It is "putnami", matching what putnamiw publishes
// (artifacts/cli/<sha>/putnami) so the Go launcher and the bash wrapper resolve
// the same shared blob, and "putnami.exe" on Windows, which starts only a
// program with that suffix and runs no putnamiw.
var cliBinaryName = cliBinaryNameFor(runtime.GOOS)

// cliBinaryNameFor is cliBinaryName on a machine running goos.
func cliBinaryNameFor(goos string) string {
	return pkgmeta.ExecutableName(goos, "putnami")
}

// CLIBinaryName is the file name a StageFunc given to AdmitCLI writes the
// executable under on this machine.
func CLIBinaryName() string {
	return cliBinaryName
}

// CLIDir returns the directory holding the prebuilt CLI for binary digest sha:
// <root>/cli/<sha>. The CLI is content-addressed by the SHA-256 of the binary
// itself — the value pinned in LockFile.CLI.Integrities["os/arch"] — in a tree
// separate from the sha256/ artifacts and at the same path putnamiw publishes,
// so both resolvers share one cache.
func (s *Store) CLIDir(sha string) string {
	return filepath.Join(s.root, cliDirName, sha)
}

// CLIBinary returns the executable path for binary digest sha:
// <root>/cli/<sha>/putnami (putnami.exe on Windows).
func (s *Store) CLIBinary(sha string) string {
	return filepath.Join(s.CLIDir(sha), cliBinaryName)
}

// HasCLI reports whether a published, executable CLI binary exists for sha. Like
// Has it returns false for an invalid digest, so an attacker-influenced path
// component never reaches the filesystem.
func (s *Store) HasCLI(sha string) bool {
	if !isHexDigest(sha) {
		return false
	}
	info, err := os.Stat(s.CLIBinary(sha))
	return err == nil && info.Mode().IsRegular()
}

// TouchCLI stamps the CLI entry's last-used time so GC's recency/grace spares
// it. Best-effort and lock-free; a missing or invalid entry is a silent no-op.
// The launcher calls this on every resolve so a pinned CLI stays warm without
// enumerating worktrees, mirroring putnamiw's touch_recency.
func (s *Store) TouchCLI(sha string) {
	if !isHexDigest(sha) {
		return
	}
	dir := s.CLIDir(sha)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	stampUsed(dir, time.Now())
}

// AdmitCLI publishes a verified CLI binary into <root>/cli/<sha>/putnami and
// returns the absolute path to the executable. stage MUST write the executable
// as CLIBinaryName() into the staging dir and verify it hashes to sha (see StageFunc):
// the blast radius of a poisoned CLI is machine-wide, so publish happens only on
// stage success. First-writer-wins with putnamiw's own publisher — both target
// cli/<sha>/, so whichever runs first wins and the other reuses it.
func (s *Store) AdmitCLI(sha string, stage StageFunc) (string, error) {
	if !isHexDigest(sha) {
		return "", fmt.Errorf("artifactstore: invalid cli digest %q", sha)
	}
	if _, err := s.admit(context.Background(), sha, s.CLIDir(sha), stage); err != nil {
		return "", err
	}
	return s.CLIBinary(sha), nil
}
