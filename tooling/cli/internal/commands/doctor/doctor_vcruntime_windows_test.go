//go:build windows

package doctor

import (
	"testing"

	doctor "go.putnami.dev/protocol/doctor"
)

// TestVCRuntimeInstalled_MatchesTheHostCheck looks for the real
// vcruntime140.dll and checks that the host probe reports the runtime missing
// exactly when it is absent. It runs on the Windows QA host.
func TestVCRuntimeInstalled_MatchesTheHostCheck(t *testing.T) {
	installed, err := vcRuntimeInstalled()
	if err != nil {
		t.Fatalf("vcRuntimeInstalled: %v", err)
	}
	got := countCode(checkVCRuntime(doctor.ProfileDev, systemWorkstationProbe(t.TempDir())), doctor.CheckVCRuntimeMissing)
	if installed && got != 0 {
		t.Fatalf("vcruntime140.dll is installed but the check reported %d finding(s)", got)
	}
	if !installed && got != 1 {
		t.Fatalf("vcruntime140.dll is missing but the check reported %d finding(s), want 1", got)
	}
}
