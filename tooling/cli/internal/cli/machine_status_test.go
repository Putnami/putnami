package cli

import (
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/machineproc"
)

// `putnami machine status` says how many sampled answers of the input record
// the last worker run used and the worker checked once it returned, and, when
// one differed, why the worker withdrew what it held (tooling/cli ADR 0113
// §1).
func TestMachineStatusSaysWhyTheWorkerWithdrewWhatItHeld(t *testing.T) {
	t.Parallel()
	reason := "the input record answered planted for /w/p src/**/*.go, which a collection gives as right"
	var out strings.Builder
	writeLastMemory(&out, &machineproc.MemoryDecision{
		Root: "/w", Reason: "no run of this worktree is held", Kept: "not kept: a sampled answer of the input record differed",
		Sampled: 3, SampledMillis: 41, Withdrawn: reason,
	})
	for _, want := range []string{
		"Last worker run answerable from memory: no, no run of this worktree is held; not kept: a sampled answer of the input record differed (/w)",
		"Sampled input digests checked after it returned: 3, in 41 ms",
		"Withdrawn after it returned: " + reason + "; the input record is discarded",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status says\n%s\nwant %q", out.String(), want)
		}
	}
	out.Reset()
	writeLastMemory(&out, &machineproc.MemoryDecision{Root: "/w", Answerable: true, Answered: true, Kept: "kept"})
	if strings.Contains(out.String(), "Sampled") || strings.Contains(out.String(), "Withdrawn") {
		t.Errorf("a run with no sampled answer to check says\n%s", out.String())
	}
}
