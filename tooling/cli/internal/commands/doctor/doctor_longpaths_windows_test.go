//go:build windows

package doctor

import (
	"testing"

	doctor "go.putnami.dev/protocol/doctor"
)

// TestReadLongPathsEnabled_MatchesTheHostCheck reads the real registry value
// and checks that the host probe reports long paths exactly when it is on. It
// runs on the Windows QA host.
func TestReadLongPathsEnabled_MatchesTheHostCheck(t *testing.T) {
	enabled, err := readLongPathsEnabled()
	if err != nil {
		t.Fatalf("readLongPathsEnabled: %v", err)
	}
	probe := systemWorkstationProbe(t.TempDir())
	if probe.goos != "windows" {
		t.Fatalf("probe.goos = %q, want windows", probe.goos)
	}
	got := countCode(checkLongPaths(doctor.ProfileDev, probe), doctor.CheckLongPathsDisabled)
	if enabled && got != 0 {
		t.Fatalf("LongPathsEnabled is 1 but the check reported %d finding(s)", got)
	}
	if !enabled && got != 1 {
		t.Fatalf("LongPathsEnabled is off but the check reported %d finding(s), want 1", got)
	}
}
