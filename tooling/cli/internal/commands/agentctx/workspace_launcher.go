package agentctx

import (
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// workspaceLauncher names the command that starts Putnami in wsRoot from a
// file the workspace commits. It is the bare binary name, which resolves from
// PATH on each machine, so the file is portable across the team and safe to
// commit. A source workspace (the lock records cli.source "workspace") refuses
// that global binary (ADR 0028), so when its root holds a putnamiw file the
// command is `./putnamiw`, which builds the CLI from the tree. Only committed
// content decides: the file mode is not read, because Windows reports no
// executable bit and the same commit would then have two canonical commands.
// The path is relative to the host's launch directory, the workspace root. A
// lock that cannot be read is an error: it cannot rule a source workspace out.
func workspaceLauncher(wsRoot string) (string, error) {
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return "", &launcherLockError{err: err}
	}
	if lf == nil {
		return "putnami", nil
	}
	if cli, declared := lf.GetCLI(); !declared || !cli.IsWorkspaceSource() {
		return "putnami", nil
	}
	info, err := os.Stat(filepath.Join(wsRoot, "putnamiw"))
	if err != nil || !info.Mode().IsRegular() {
		return "putnami", nil
	}
	return "./putnamiw", nil
}

// launcherLockError is a putnami.lock.json that cannot be read while choosing
// the launcher. It names the lock as the file to fix.
type launcherLockError struct{ err error }

func (e *launcherLockError) Error() string {
	msg := e.err.Error()
	if !strings.Contains(msg, lockfile.LockFilename) {
		msg = "read " + lockfile.LockFilename + ": " + msg
	}
	return msg
}

func (e *launcherLockError) Unwrap() error { return e.err }
