package procguard

import (
	"runtime"
	"testing"
)

func TestDenyInspectionDoesNothingOffLinux(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "linux" {
		t.Skip("Linux marks the process: TestDenyInspectionMarksOnlyTheCallingProcess covers it in a child process")
	}
	for range 2 {
		if err := DenyInspection(); err != nil {
			t.Fatalf("DenyInspection() = %v, want nil on %s", err, runtime.GOOS)
		}
	}
}
