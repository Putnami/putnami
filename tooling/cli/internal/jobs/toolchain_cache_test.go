package jobs

import (
	"bytes"
	"runtime"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func TestBoundedBufferKeepsAConstantAmountOfMemory(t *testing.T) {
	t.Parallel()
	b := &boundedBuffer{limit: 16}
	chunk := bytes.Repeat([]byte("x"), 1024)
	for range 4096 { // 4MB written, 16 bytes retained
		n, err := b.Write(chunk)
		// A short or failing write makes os/exec abandon the pipe copy, which
		// blocks the child forever on a full pipe.
		if n != len(chunk) || err != nil {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(chunk))
		}
	}
	if len(b.buf) != 16 {
		t.Fatalf("retained %d bytes, want the %d-byte cap", len(b.buf), 16)
	}
	if !b.truncated {
		t.Fatal("truncation went unrecorded, so oversized output would read as a version")
	}
}

func TestBoundedBufferKeepsShortOutputWhole(t *testing.T) {
	t.Parallel()
	b := &boundedBuffer{limit: 16}
	if _, err := b.Write([]byte("1.2.3\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := string(b.buf); got != "1.2.3\n" {
		t.Fatalf("buffer = %q, want the output verbatim", got)
	}
	if b.truncated {
		t.Fatal("output within the cap was marked truncated")
	}
}

func TestCacheToolchainVersionIncludesResolvedRuntimeRequirements(t *testing.T) {
	t.Parallel()

	ext := &extension.ExtensionDescription{RuntimeToolchains: map[string]model.RuntimeToolchainResolution{
		"compiler": {Available: true, Identity: "lock-a"},
	}}
	job := &ScheduledJob{Extension: ext, JobDef: &extension.JobDefinition{Toolchains: []string{"compiler"}}}
	first := cacheToolchainVersion(job)
	ext.RuntimeToolchains["compiler"] = model.RuntimeToolchainResolution{Available: true, Identity: "lock-b"}
	second := cacheToolchainVersion(job)

	if want := runtime.Version() + ";runtime-tools=compiler=lock-a"; first != want {
		t.Fatalf("runtime toolchain key = %q, want %q", first, want)
	}
	if first == second {
		t.Fatalf("runtime toolchain key did not vary after a locked identity change: %q", first)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "identity-keys-results",
		"a-task-cache-key-follows-the-resolved-identity")
}

func TestCacheToolchainVersionLeavesTasksWithoutRequirementsStable(t *testing.T) {
	t.Parallel()

	job := &ScheduledJob{Extension: &extension.ExtensionDescription{Name: "example"}, JobDef: &extension.JobDefinition{}}
	if got := cacheToolchainVersion(job); got != runtime.Version() {
		t.Fatalf("task toolchain cache key = %q, want %q", got, runtime.Version())
	}
}
