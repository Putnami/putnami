package machine

import (
	"encoding/json"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// The runner environment's projection and the run's CPU balance.
//
// An earlier slice proved cost is countable. These tests pin what makes it INTERPRETABLE:
// the run states its actual CPU against what the runner allocated, actual is
// summed from the physical ledger so a batch counts once, and a platform that
// measures nothing publishes nothing rather than a reassuring zero.

// quotaLimitedSession is the batched run from the ledger tests, placed on a
// container throttled to 8 cores: five logical rows, two physical executions,
// 12s of measured CPU over a 3s wall.
func quotaLimitedSession(t *testing.T) Run {
	t.Helper()
	run := batchedRun()
	run.Session.Duration = 3 * time.Second
	run.Session.Environment = &jobs.RunnerEnvironment{
		OS:          "linux",
		Arch:        "amd64",
		LogicalCPUs: 16,
		CPUModel:    "AMD EPYC 7B13 64-Core Processor",
		Window:      3020 * time.Millisecond,
		Cgroup: &jobs.CgroupCPU{
			PeriodUs: 100000,
			QuotaUs:  800000,
			UsageUs:  12800000,
			HasUsage: true,
			Throttle: &jobs.CgroupThrottle{Periods: 30, ThrottledPeriods: 7, ThrottledUs: 210000},
		},
		CPUPressure: &jobs.CPUPressure{SomeStalledUs: 412000},
		HostCPU:     &jobs.HostCPUTime{StealTicks: 20, IOWaitTicks: 10, TotalTicks: 130},
	}
	return run
}

// TestRunCPU_CountsPhysicalWorkOnce is criterion 5. The five logical rows
// include three sharing one subprocess, so a task-derived total would inflate
// the actual figure by the batch factor — and the epic's whole savings
// arithmetic is measured against this number.
func TestRunCPU_CountsPhysicalWorkOnce(t *testing.T) {
	run := quotaLimitedSession(t)
	summary := run.Summary(3000)

	cpu := summary.CPU
	if cpu == nil {
		t.Fatal("no CPU balance for a run with executions and a captured environment")
	}
	if cpu.AllocatedMillicores != 8000 || cpu.AllocatedSource != protocolcli.CPUAllocationCgroupQuota {
		t.Errorf("allocation = %d millicores from %q, want 8000 from the cgroup quota",
			cpu.AllocatedMillicores, cpu.AllocatedSource)
	}
	// 3000ms of wall at 8 cores.
	if cpu.AllocatedMs != 24000 {
		t.Errorf("allocatedMs = %d, want 24000", cpu.AllocatedMs)
	}
	// 9s+2s for the batch, 0.9s+0.1s for the solo spawn. The three batch members
	// contribute their execution ONCE.
	if cpu.ActualMs != 12000 || cpu.Executions != 2 {
		t.Errorf("actual = %dms over %d executions, want 12000 over 2", cpu.ActualMs, cpu.Executions)
	}

	// Adding a sixth logical row to the same subprocess changes the fan-out and
	// must not move the physical total by a millisecond.
	shared := run.Tasks[0].Result.Execution
	run.Tasks = append(run.Tasks, Task{
		Identity: identityOfKey("/f:lint"),
		Result:   jobs.TaskResult{Status: jobs.TaskStatusSuccess, Execution: shared},
	})
	if after := run.Summary(3000).CPU; after.ActualMs != cpu.ActualMs || after.Executions != cpu.Executions {
		t.Errorf("physical CPU moved with logical fan-out: %+v → %+v", *cpu, *after)
	}
}

// TestRunCPU_AbsentWithoutAnEnvironment: allocation needs a measured
// denominator. Without one the block is omitted rather than filled from a
// guess, because every utilization ratio downstream would inherit the guess and
// nothing on the wire would say so.
func TestRunCPU_AbsentWithoutAnEnvironment(t *testing.T) {
	run := quotaLimitedSession(t)
	run.Session.Environment = nil
	if cpu := run.Summary(3000).CPU; cpu != nil {
		t.Errorf("CPU balance published without an environment: %+v", *cpu)
	}
}

// TestRunCPU_AbsentWhenNothingExecuted: an all-cached run spent no machine
// time, and the absence of the block is what says so. A zero would be a claim.
func TestRunCPU_AbsentWhenNothingExecuted(t *testing.T) {
	run := quotaLimitedSession(t)
	run.Session.Executions = nil
	if cpu := run.Summary(3000).CPU; cpu != nil {
		t.Errorf("CPU balance published for a run that spawned nothing: %+v", *cpu)
	}
}

// TestRunCPU_FallsBackToTheVisibleCoreCount: with no quota the allocation is
// the core count, and it SAYS so. Conflating the two would read throttling into
// a machine that was merely busy.
func TestRunCPU_FallsBackToTheVisibleCoreCount(t *testing.T) {
	run := quotaLimitedSession(t)
	run.Session.Environment.Cgroup = nil
	cpu := run.Summary(3000).CPU
	if cpu == nil {
		t.Fatal("no CPU balance for a run on an unconstrained machine")
	}
	if cpu.AllocatedMillicores != 16000 || cpu.AllocatedSource != protocolcli.CPUAllocationLogicalCPUs {
		t.Errorf("allocation = %d millicores from %q, want 16000 from the visible cores",
			cpu.AllocatedMillicores, cpu.AllocatedSource)
	}
	if cpu.AllocatedMs != 48000 {
		t.Errorf("allocatedMs = %d, want 48000", cpu.AllocatedMs)
	}
}

// TestSessionFile_EnvironmentValidatesAgainstTheContract runs the projection
// through the protocol's own validator, so the allocated-vs-wall tie and the
// /proc/stat column rules are proven on the document this CLI actually writes.
func TestSessionFile_EnvironmentValidatesAgainstTheContract(t *testing.T) {
	run := quotaLimitedSession(t)
	run.Session.Environment.MemoryCapacity = &jobs.MemoryCapacity{
		PhysicalBytes:   32 << 30,
		CgroupLimit:     &jobs.CgroupMemoryLimit{Bytes: 16 << 30, Source: jobs.CgroupMemoryV2},
		EffectiveBytes:  16 << 30,
		EffectiveSource: jobs.MemoryCapacityCgroupLimit,
	}
	run.Session.Environment.CgroupMemory = &jobs.CgroupMemory{
		Source: jobs.CgroupMemoryV2,
		Closing: jobs.CgroupMemoryClosing{
			CurrentBytes: 8 << 30,
			Composition: &jobs.CgroupMemoryComposition{
				AnonBytes: 6 << 30, FileBytes: 2 << 30, ShmemBytes: 256 << 20,
			},
			LifetimePeak: &jobs.CgroupMemoryLifetimePeak{Bytes: 12 << 30},
		},
		Events: &jobs.CgroupMemoryEvents{High: 4, Max: 1, OOM: 1},
	}
	run.Session.Environment.MemoryPressure = &jobs.MemoryPressure{
		Scope: jobs.MemoryPressureCgroup, SomeStalledUs: 520000, FullStalledUs: 18000,
	}
	file := run.SessionFile([]string{"lint"}, nil, nil)
	file.SessionID = "20260805-141500-abc123"
	file.StartTime = "2026-08-05T14:15:00.000Z"

	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal session file: %v", err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("recorded session file violates the contract: %v\n%s", violations, data)
	}

	environment := file.Environment
	if environment == nil {
		t.Fatal("session file carries no environment")
	}
	if environment.WindowMs != 3020 {
		t.Errorf("windowMs = %d, want the sample window 3020", environment.WindowMs)
	}
	if environment.CgroupCPU == nil || environment.CgroupCPU.QuotaUs != 800000 || environment.CgroupCPU.UsageUs != 12800000 {
		t.Errorf("cgroup block = %+v", environment.CgroupCPU)
	}
	throttle := environment.CgroupCPU.Throttle
	if throttle == nil || throttle.ThrottledPeriods != 7 || throttle.Periods != 30 {
		t.Errorf("throttle block = %+v", throttle)
	}
	if environment.CPUPressure == nil || environment.CPUPressure.SomeStalledUs != 412000 {
		t.Errorf("pressure block = %+v", environment.CPUPressure)
	}
	if environment.HostCPU == nil || environment.HostCPU.StealTicks != 20 {
		t.Errorf("host CPU block = %+v", environment.HostCPU)
	}
	if environment.MemoryCapacity == nil || environment.MemoryCapacity.EffectiveBytes != 16<<30 ||
		environment.MemoryCapacity.CgroupLimit == nil ||
		environment.MemoryCapacity.CgroupLimit.Source != protocolcli.CgroupMemoryV2 {
		t.Errorf("memory capacity = %+v", environment.MemoryCapacity)
	}
	if environment.CgroupMemory == nil || environment.CgroupMemory.Closing.CurrentBytes != 8<<30 ||
		environment.CgroupMemory.Events == nil || environment.CgroupMemory.Events.High != 4 {
		t.Errorf("cgroup memory = %+v", environment.CgroupMemory)
	}
	if environment.MemoryPressure == nil || environment.MemoryPressure.Scope != protocolcli.MemoryPressureCgroup {
		t.Errorf("memory pressure = %+v", environment.MemoryPressure)
	}
}

func TestSessionFile_EnvironmentOmitsImpossibleMemorySubfacts(t *testing.T) {
	run := quotaLimitedSession(t)
	run.Session.Environment.CgroupMemory = &jobs.CgroupMemory{
		Source: jobs.CgroupMemoryV2,
		Closing: jobs.CgroupMemoryClosing{
			CurrentBytes: 1024,
			Composition:  &jobs.CgroupMemoryComposition{AnonBytes: 512, FileBytes: 128, ShmemBytes: 256},
		},
	}
	run.Session.Environment.MemoryPressure = &jobs.MemoryPressure{
		Scope: jobs.MemoryPressureCgroup, SomeStalledUs: 10, FullStalledUs: 11,
	}

	environment := run.Environment()
	if environment == nil || environment.CgroupMemory == nil {
		t.Fatal("valid cgroup closing gauge was dropped with invalid optional facts")
	}
	if environment.CgroupMemory.Closing.Composition != nil {
		t.Errorf("composition with shmem > file was projected: %+v", environment.CgroupMemory.Closing.Composition)
	}
	if environment.MemoryPressure != nil {
		t.Errorf("PSI block with full > some was projected: %+v", environment.MemoryPressure)
	}
}

// TestSessionFile_EnvironmentDegradesOnDarwin is criterion 3 at the document
// level: a machine with no cgroup, no PSI and no /proc/stat writes the facts it
// has and mentions nothing else. The blocks are absent, not zeroed — a zeroed
// throttle block would claim this machine measured no throttling, which is a
// different statement from being unable to.
func TestSessionFile_EnvironmentDegradesOnDarwin(t *testing.T) {
	run := quotaLimitedSession(t)
	run.Session.Environment = &jobs.RunnerEnvironment{
		OS:          "darwin",
		Arch:        "arm64",
		LogicalCPUs: 12,
		CPUModel:    "Apple M2 Max",
		Window:      3020 * time.Millisecond,
	}
	file := run.SessionFile([]string{"lint"}, nil, nil)
	file.SessionID = "s"
	file.StartTime = "t"

	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal session file: %v", err)
	}
	for _, member := range []string{"cgroupCpu", "cpuPressure", "hostCpu", "throttle"} {
		if bytesHave(data, member) {
			t.Errorf("darwin session claims %q: %s", member, data)
		}
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("darwin session file violates the contract: %v", violations)
	}
	if file.Run.CPU == nil || file.Run.CPU.AllocatedSource != protocolcli.CPUAllocationLogicalCPUs {
		t.Errorf("darwin run allocation = %+v, want the visible core count", file.Run.CPU)
	}
}

// TestSessionFile_UnlimitedQuotaOmitsQuotaUs: cpu.max "max" is unlimited, and
// the projection spells that by omission. Writing 0 would make an unbounded
// runner look totally starved.
func TestSessionFile_UnlimitedQuotaOmitsQuotaUs(t *testing.T) {
	run := quotaLimitedSession(t)
	run.Session.Environment.Cgroup.QuotaUs = 0
	file := run.SessionFile([]string{"lint"}, nil, nil)
	file.SessionID = "s"
	file.StartTime = "t"

	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal session file: %v", err)
	}
	if bytesHave(data, "quotaUs") {
		t.Errorf("unlimited cgroup still emitted quotaUs: %s", data)
	}
	// The throttle block survives: an unlimited cgroup still counts periods, and
	// "0 of 30 periods throttled" is exactly the finding that rules out quota
	// starvation.
	if !bytesHave(data, "throttledPeriods") {
		t.Errorf("throttle measurement dropped with the quota: %s", data)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("unlimited-quota session file violates the contract: %v", violations)
	}
}

// TestSessionFile_OmitsAnAbsentEnvironment keeps the slice additive: a
// reduction that captured nothing writes exactly the document W0a wrote.
func TestSessionFile_OmitsAnAbsentEnvironment(t *testing.T) {
	run := quotaLimitedSession(t)
	run.Session.Environment = nil
	file := run.SessionFile([]string{"lint"}, nil, nil)
	file.SessionID = "s"
	file.StartTime = "t"

	if file.Environment != nil {
		t.Errorf("environment projected from nothing: %+v", *file.Environment)
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal session file: %v", err)
	}
	for _, member := range []string{"environment", "\"cpu\""} {
		if bytesHave(data, member) {
			t.Errorf("absent capture still emitted %s: %s", member, data)
		}
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("environment-free session file violates the contract: %v", violations)
	}
}
