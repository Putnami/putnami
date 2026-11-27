package sessionreporter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/filelock"
)

// PendingSessions names, newest first and at most limit of them, the
// finalized sessions whose checkpoint of a selected capability, for the
// selected provider, is incomplete, and whose lock no process holds. A
// complete checkpoint, another provider's, an unreadable one, an unfinalized
// session, a session a replay holds, the excluded session and the latest
// pointer are not owed.
func TestPendingSessionsAreTheNewestIdleFinalizedIncompleteCheckpoints(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "detached-delivery", "an-interrupted-delivery-reaches-the-receiver-on-a-later-run-exactly-once")
	t.Setenv(protocolcli.SessionReporterEnv, "@test/provider")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "")
	t.Setenv(protocolcli.LogReporterEnv, "")
	t.Setenv(protocolcli.LogReporterTokenEnv, "")
	ctx := Capture(context.Background())
	root := t.TempDir()
	unfinalized := map[string]bool{}
	finalized := finalizedSession
	finalizedSession = func(_ string, id string) error {
		if unfinalized[id] {
			return errors.New("no finalized record")
		}
		return nil
	}
	t.Cleanup(func() { finalizedSession = finalized })
	write := func(id, provider string, complete bool) string {
		t.Helper()
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		data := `{"version":2,"provider":"` + provider + `","sessionId":"` + id + `","complete":` + map[bool]string{true: "true", false: "false"}[complete] + `}`
		if err := os.WriteFile(filepath.Join(dir, StateFile), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, SessionReporter.LockFile), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	write("20261127-090000-aaaaaa", "@test/provider", false)
	write("20261127-100000-aaaaaa", "@test/provider", false)
	write("20261127-110000-bbbbbb", "@test/provider", false)
	write("20261127-120000-cccccc", "@test/provider", true)
	write("20261127-130000-dddddd", "@test/other", false)
	unreadable := write("20261127-140000-eeeeee", "@test/provider", false)
	if err := os.WriteFile(filepath.Join(unreadable, StateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("20261127-143000-killed", "@test/provider", false)
	unfinalized["20261127-143000-killed"] = true
	held := write("20261127-144500-active", "@test/provider", false)
	lock, err := filelock.OpenFile(filepath.Join(held, SessionReporter.LockFile), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := filelock.LockFile(lock, true, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = filelock.UnlockFile(lock); _ = lock.Close() })
	write("20261127-150000-ffffff", "@test/provider", false)
	if err := os.Mkdir(filepath.Join(root, "20261127-080000-nostat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "20261127-110000-bbbbbb"), filepath.Join(root, "latest")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := PendingSessions(ctx, root, "20261127-150000-ffffff", 2)
	if want := []string{"20261127-110000-bbbbbb", "20261127-100000-aaaaaa"}; !slices.Equal(got, want) {
		t.Fatalf("pending = %v, want %v", got, want)
	}
	if got := PendingSessions(ctx, root, "", 1); !slices.Equal(got, []string{"20261127-150000-ffffff"}) {
		t.Fatalf("newest pending = %v", got)
	}
	if got := PendingSessions(ctx, filepath.Join(root, "missing"), "", 3); got != nil {
		t.Fatalf("a missing root owes %v", got)
	}
}
