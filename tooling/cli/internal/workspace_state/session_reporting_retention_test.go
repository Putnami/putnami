package workspace_state

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/flock"
)

// TestReportingCheckpointsNameEveryCapability pins the checkpoint and lock of
// every reporting capability; internal/sessionreporter's capability table
// pins the same names.
func TestReportingCheckpointsNameEveryCapability(t *testing.T) {
	want := []struct{ state, lock string }{
		{"reporting.json", "reporting.lock"},
		{"log-reporting.json", "log-reporting.lock"},
	}
	if len(reportingCheckpoints) != len(want) {
		t.Fatalf("reporting checkpoints = %+v, want %+v", reportingCheckpoints, want)
	}
	for i, checkpoint := range reportingCheckpoints {
		if checkpoint != want[i] {
			t.Fatalf("reporting checkpoint %d = %+v, want %+v", i, checkpoint, want[i])
		}
	}
}

func TestReportingRetentionIsBoundedAndPreservesActiveReplay(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "bounded-transport", "pending-retention-honors-session-policy-and-active-locks")
	for _, checkpoint := range reportingCheckpoints {
		t.Run(checkpoint.state, func(t *testing.T) {
			testRetentionPreservesActiveReplay(t, checkpoint.state, checkpoint.lock)
		})
	}
}

// testRetentionPreservesActiveReplay bounds the retention of one capability's
// pending sessions and never prunes the one whose lock is held.
func testRetentionPreservesActiveReplay(t *testing.T, stateFile, lockFile string) {
	store := NewSessionStoreWithRetention(t.TempDir(), 2)
	for i := 0; i < 4; i++ {
		dir := filepath.Join(store.Root(), sessionIDForTest(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"complete":false}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	active := filepath.Join(store.Root(), sessionIDForTest(0), lockFile)
	lock, err := flock.OpenFile(active, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := flock.LockFile(lock, true, true); err != nil {
		t.Fatal(err)
	}
	// Model the lock-to-first-checkpoint window of a newly started reporter.
	if err := os.Remove(filepath.Join(filepath.Dir(active), stateFile)); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	ids, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("outage grew retention to %d", len(ids))
	}
	if _, err := os.Stat(filepath.Dir(active)); err != nil {
		t.Fatal("active replay was pruned")
	}
	if err := flock.UnlockFile(lock); err != nil {
		t.Fatal(err)
	}
	store.maxSessions = 1
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(active)); !os.IsNotExist(err) {
		t.Fatal("inactive oldest pending session never expired")
	}
}

// TestReportingRetentionPrefersPendingOverCompletedHistory keeps the older
// session whose delivery is pending for any capability, even when the other
// capability completed it.
func TestReportingRetentionPrefersPendingOverCompletedHistory(t *testing.T) {
	for _, pending := range reportingCheckpoints {
		t.Run(pending.state, func(t *testing.T) {
			store := NewSessionStoreWithRetention(t.TempDir(), 1)
			for i := 0; i < 2; i++ {
				dir := filepath.Join(store.Root(), sessionIDForTest(i))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, checkpoint := range reportingCheckpoints {
					state := `{"complete":true}`
					if i == 0 && checkpoint == pending {
						state = `{"complete":false}`
					}
					if err := os.WriteFile(filepath.Join(dir, checkpoint.state), []byte(state), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.Prune(); err != nil {
				t.Fatal(err)
			}
			ids, err := store.List()
			if err != nil || len(ids) != 1 || ids[0] != sessionIDForTest(0) {
				t.Fatalf("pending retention=%v error=%v", ids, err)
			}
		})
	}
}
