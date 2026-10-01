package cli

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// TestRetryFailedIsAnExecutionPolicyFlag pins --retry-failed on all three
// surfaces a global flag has to reach at once — the parser, the help/completion
// catalog, and the run-shaping flags the engine reads — and pins what it must
// NOT reach: the flag is execution policy, exactly like --max-parallel, so it
// never becomes a command parameter and therefore never moves a cache key.
func TestRetryFailedIsAnExecutionPolicyFlag(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"retry-failed-is-execution-policy-only")

	parsed := ParseArgs([]string{"test", "--retry-failed"}, nil, nil)
	if !parsed.Global.RetryFailed {
		t.Fatal("--retry-failed did not reach the run-shaping flags")
	}
	if parsed.Global.NoCache || parsed.Global.NoCacheExplicit {
		t.Error("--retry-failed implied --no-cache: it forces re-execution, it does not disable the cache")
	}
	// The flag is consumed by the global parser, so nothing hands it to a job
	// subprocess or to the params a cache key is computed from.
	for _, arg := range parsed.RawJobArgs {
		if strings.Contains(arg, "retry-failed") {
			t.Errorf("--retry-failed leaked into the job arguments: %v", parsed.RawJobArgs)
		}
	}
	for name := range parsed.JobFlags {
		if strings.Contains(name, "retry-failed") {
			t.Errorf("--retry-failed leaked into the job flags: %v", parsed.JobFlags)
		}
	}

	// The catalog row is what help and every shell completion read.
	var found *commandmeta.GlobalFlag
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.Long == "--retry-failed" {
			found = &flag
			break
		}
	}
	if found == nil {
		t.Fatal("--retry-failed is absent from the global-flag catalog")
	}
	if found.Type == commandmeta.FlagValue {
		t.Error("--retry-failed must be a presence flag, not a value flag")
	}
	if found.Category != "Execution" {
		t.Errorf("--retry-failed category = %q, want Execution", found.Category)
	}

	// Without the flag the default stands: a recorded failure is replayed.
	if ParseArgs([]string{"test"}, nil, nil).Global.RetryFailed {
		t.Error("--retry-failed defaulted to on")
	}
}
