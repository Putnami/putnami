package versioncmd

import (
	"context"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// RefreshLockMetadataWithResult stamps the workspace lock's CLI pin with the
// running binary's output protocol version when the pin names that binary's
// version, and reports whether the lock file was replaced. A lock below
// format version 3, which records no protocol version, is stamped at version
// 3. Without a lock it writes a new one. It skips the atomic rewrite when the
// canonical bytes already match the file on disk.
func RefreshLockMetadataWithResult(_ context.Context, wsRoot, runningVersion string) (bool, error) {
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return false, err
	}
	if lf == nil {
		lf = lockfile.NewLockFile()
	}
	// A source workspace pins no published CLI, so there is no artifact whose
	// output protocol could be recorded: stamping protocolVersion onto the
	// sentinel would invent a claim about bytes that do not exist, and rewriting
	// the entry would erase the sentinel a committed lock carries. Leave it
	// exactly as written. (sameCLIVersion already rejects the empty version a
	// sentinel carries; the explicit guard states the invariant instead of
	// relying on that coincidence.)
	if cli, ok := lf.GetCLI(); ok && !cli.IsWorkspaceSource() && sameCLIVersion(cli.Version, runningVersion) {
		cli.ProtocolVersion = protocolcli.ResultProtocolVersion
		lf.SetCLI(cli)
		if lf.Version < lockfile.FormatVersionV3 {
			lf.Version = lockfile.FormatVersionV3
		}
	}
	return lockfile.WriteLockFileIfChanged(wsRoot, lf)
}

// sameCLIVersion reports whether a and b name the same CLI release, ignoring a
// leading "v" and surrounding whitespace either may carry.
func sameCLIVersion(a, b string) bool {
	a = strings.TrimPrefix(strings.TrimSpace(a), "v")
	b = strings.TrimPrefix(strings.TrimSpace(b), "v")
	return a != "" && a == b
}
