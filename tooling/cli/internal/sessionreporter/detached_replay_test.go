package sessionreporter

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// Only a run that started a reporter, finalized its session, and is neither
// hosted nor in CI detaches its reporting; a hosted run starts no detached
// replay at all.
func TestOnlyALocalRunWithAFinalizedSessionDetachesItsReporting(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "detached-delivery", "hosted-and-ci-runs-drain-before-they-return")
	t.Setenv("CI", "")
	for _, marker := range ciMarkers {
		t.Setenv(marker, "")
	}
	if !Detaches(true, true) {
		t.Fatal("a local run drains its reporting")
	}
	if Detaches(false, true) || Detaches(true, false) {
		t.Fatal("a run without a reporter, or without a finalized session, detaches its reporting")
	}
	t.Setenv("CI", "true")
	if Detaches(true, true) {
		t.Fatal("a CI run detaches its reporting")
	}
	t.Setenv("CI", "")
	for _, marker := range ciMarkers {
		t.Setenv(marker, "set")
		if Detaches(true, true) {
			t.Fatalf("a CI job that sets %s detaches its reporting", marker)
		}
		t.Setenv(marker, "")
	}
	restore := runcredential.SetForTest("hosted-run-credential")
	defer restore()
	if Detaches(true, true) {
		t.Fatal("a hosted run detaches its reporting")
	}
	if _, err := replayCommand(t.Context(), t.TempDir(), "20261127-120000-abcdef"); err == nil {
		t.Fatal("a hosted run built a detached replay")
	}
}
