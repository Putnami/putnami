package extensions

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// A hosted install fails when the lock it would write differs from the
// committed one, such as for a configured extension the lock does not pin,
// and names the fix. The committed lock alone passes, and a run without the
// run credential or a materialization is never refused.
func TestAHostedInstallRefusesALockChange(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "cold-install-leaves-locks-unchanged", "hosted-install-writes-no-lock")
	wsRoot := t.TempDir()
	committed := lockfile.NewLockFile()
	committed.SetExtension("@acme/pinned", lockfile.LockEntry{Version: "1.0.0"})
	if err := lockfile.WriteLockFile(wsRoot, committed); err != nil {
		t.Fatal(err)
	}
	unpinned := committed.Clone()
	unpinned.SetExtension("@acme/unpinned", lockfile.LockEntry{Version: "2.0.0"})
	ops := extensionOps(wsRoot)
	materialize := ops
	materialize.materialize = true

	for _, c := range []struct {
		name      string
		bearer    string
		requested *lockfile.LockFile
		ops       artifactOps
		refused   bool
	}{
		{name: "hosted, the committed lock", bearer: "run-bearer", requested: committed.Clone(), ops: ops},
		{name: "hosted, an unpinned extension", bearer: "run-bearer", requested: unpinned, ops: ops, refused: true},
		{name: "hosted materialization", bearer: "run-bearer", requested: unpinned, ops: materialize},
		{name: "flag off, an unpinned extension", requested: unpinned, ops: ops},
	} {
		restore := runcredential.SetForTest(c.bearer)
		err := refuseHostedLockChange(wsRoot, c.requested, c.ops)
		persists := c.ops.persistsLock(t.Context())
		restore()
		if c.refused != (err != nil) {
			t.Errorf("%s: refuseHostedLockChange = %v, want refused %v", c.name, err, c.refused)
		}
		if err != nil && !strings.Contains(err.Error(), "run `putnami install` without "+runcredential.Flag) {
			t.Errorf("%s: the refusal %q does not name the fix", c.name, err)
		}
		if c.bearer != "" && persists {
			t.Errorf("%s: a hosted install persists the lock", c.name)
		}
	}
}
