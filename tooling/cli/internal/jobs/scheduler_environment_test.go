package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs/machineprobe"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// fixtureRunner writes one runner's procfs/sysfs surface under a temp root.
func fixtureRunner(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// TestPrepareScheduling_ClampsAllocatorToCapturedCgroupQuota: the opening
// sample is a runner with 8 GiB of physical memory, in a cgroup that permits
// 2.5 CPUs and 2 GiB. The sample is captured from a fixture tree because its
// fields belong to machineprobe.
func TestPrepareScheduling_ClampsAllocatorToCapturedCgroupQuota(t *testing.T) {
	t.Parallel()
	scheduler := &Scheduler{ws: &workspace.Workspace{Root: t.TempDir()}}
	scheduler.environmentBegin = machineprobe.CaptureEnvironmentSample(fixtureRunner(t, map[string]string{
		"proc/meminfo":                    "MemTotal:        8388608 kB\n",
		"proc/self/cgroup":                "0::/runner\n",
		"sys/fs/cgroup/runner/cpu.max":    "250000 100000\n",
		"sys/fs/cgroup/runner/memory.max": "2147483648\n",
	}))
	memoryCapacity, _, ok := scheduler.environmentBegin.EffectiveMemoryBytes()
	if !ok {
		t.Fatal("opening memory capacity did not resolve")
	}
	decision := resolveParallelDecisionWithHardware(SchedulerConfig{}, nil, 12, memoryCapacity)
	scheduler.prepareScheduling(decision)

	if got := scheduler.cpuAlloc.totalCPU; got != 2 {
		t.Fatalf("allocator capacity = %d, want captured quota floor 2 (not logical CPUs or GOMAXPROCS)", got)
	}
	if got := scheduler.resources.cpuCapacity; got != 2 {
		t.Fatalf("admission capacity = %d, want the same captured quota floor 2", got)
	}
	if decision.MemoryTotalMiB != 2048 || decision.MemoryUsableMiB != 1536 {
		t.Fatalf("memory decision = total %d / usable %d MiB, want 2048 / 1536",
			decision.MemoryTotalMiB, decision.MemoryUsableMiB)
	}
	if got := scheduler.resources.memoryCapacity; got != 1536*1024*1024 {
		t.Fatalf("memory admission = %d, want the unchanged 75%% of opening 2 GiB", got)
	}
}
