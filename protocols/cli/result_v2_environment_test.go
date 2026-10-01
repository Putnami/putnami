package cli

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The runner environment and the run's CPU balance.
//
// The execution ledger made physical cost countable. This makes it
// INTERPRETABLE: the same
// 80 CPU-minutes mean something different on a 16-core host with no quota than
// on a container throttled to 8, and different again on a host whose neighbors
// are stealing a fifth of every tick. These tests pin the two properties that
// distinction rests on — the block round-trips without losing a member, and a
// platform that cannot measure something says NOTHING rather than zero.

func sessionFileWithEnvironment() SessionFile {
	return SessionFile{
		ProtocolVersion: ResultProtocolVersion,
		SessionID:       "20260805-141500-abc123",
		StartTime:       "2026-08-05T14:15:00.000Z",
		EndTime:         "2026-08-05T14:15:03.500Z",
		Commands:        []string{"build"},
		Run: RunSummary{
			Outcome:    RunOutcomeSuccess,
			ExitCode:   ExitSuccess,
			Counts:     RunCounts{Total: 1, Succeeded: 1},
			DurationMs: 3500,
			CPU: &RunCPU{
				AllocatedMillicores: 8000,
				AllocatedSource:     CPUAllocationCgroupQuota,
				AllocatedMs:         28000,
				ActualMs:            18000,
				Executions:          1,
			},
		},
		Executions: []ExecutionRecord{
			{ID: "exec-1", WallMs: 3000, UserCPUMs: 16000, SystemCPUMs: 2000, Tasks: 1},
		},
		Environment: &SessionEnvironment{
			OS:          "linux",
			Arch:        "amd64",
			LogicalCPUs: 16,
			CPUModel:    "AMD EPYC 7B13",
			WindowMs:    3520,
			CgroupCPU: &CgroupCPU{
				PeriodUs: 100000,
				QuotaUs:  800000,
				UsageUs:  22140000,
				Throttle: &CgroupThrottle{Periods: 35, ThrottledPeriods: 4, ThrottledUs: 128000},
			},
			CPUPressure: &CPUPressure{SomeStalledUs: 412000},
			HostCPU:     &HostCPUTime{StealTicks: 12, IOWaitTicks: 3, TotalTicks: 2816},
			MemoryCapacity: &MemoryCapacity{
				PhysicalBytes: 32 * 1024 * 1024 * 1024,
				CgroupLimit: &CgroupMemoryLimit{
					Bytes:  16 * 1024 * 1024 * 1024,
					Source: CgroupMemoryV2,
				},
				EffectiveBytes:  16 * 1024 * 1024 * 1024,
				EffectiveSource: MemoryCapacityCgroupLimit,
			},
			CgroupMemory: &CgroupMemory{
				Source: CgroupMemoryV2,
				Closing: CgroupMemoryClosing{
					CurrentBytes: 8 * 1024 * 1024 * 1024,
					Composition: &CgroupMemoryComposition{
						AnonBytes:  6 * 1024 * 1024 * 1024,
						FileBytes:  2 * 1024 * 1024 * 1024,
						ShmemBytes: 256 * 1024 * 1024,
					},
					LifetimePeak: &CgroupMemoryLifetimePeak{Bytes: 12 * 1024 * 1024 * 1024},
				},
				Events: &CgroupMemoryEvents{Low: 2, High: 4, Max: 1, OOM: 1, OOMKill: 0},
			},
			MemoryPressure: &MemoryPressure{
				Scope: MemoryPressureCgroup, SomeStalledUs: 520000, FullStalledUs: 18000,
			},
		},
	}
}

// TestSessionEnvironmentRoundTrip pins the whole block through the wire: every
// member survives marshal/unmarshal, and the document validates.
func TestSessionEnvironmentRoundTrip(t *testing.T) {
	want := sessionFileWithEnvironment()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SessionFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip lost a member:\n got: %+v\nwant: %+v", got, want)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionEnvironmentIsAdditive keeps an older reader whole: a document
// with no environment and no run.cpu is exactly the document the ledger-only
// producer wrote, and it
// still validates.
func TestSessionEnvironmentIsAdditive(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment = nil
	file.Run.CPU = nil
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, member := range []string{"environment", "\"cpu\""} {
		if bytesContain(data, member) {
			t.Errorf("absent block still emits %s: %s", member, data)
		}
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionEnvironmentDegradesToAbsence pins the absence rule, stated on
// the wire: a macOS machine has no cgroup, no PSI and no /proc/stat, and the
// contract records that as three ABSENT objects. A zeroed cgroup block would
// read as "measured: never throttled" for a machine that cannot throttle at
// all, which is a false measurement, not a conservative one.
func TestSessionEnvironmentDegradesToAbsence(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment = &SessionEnvironment{
		OS:          "darwin",
		Arch:        "arm64",
		LogicalCPUs: 12,
		CPUModel:    "Apple M2 Max",
		WindowMs:    3520,
	}
	file.Run.CPU = &RunCPU{
		AllocatedMillicores: 12000,
		AllocatedSource:     CPUAllocationLogicalCPUs,
		AllocatedMs:         42000,
		ActualMs:            18000,
		Executions:          1,
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, member := range []string{
		"cgroupCpu", "cpuPressure", "hostCpu", "throttle",
		"memoryCapacity", "cgroupMemory", "memoryPressure",
	} {
		if bytesContain(data, member) {
			t.Errorf("darwin document claims %s: %s", member, data)
		}
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionMemoryMeasuredZeros pins the presence rule for every optional
// memory measurement: a zero closing gauge, lifetime peak, composition, event
// delta or PSI delta is evidence, not absence.
func TestSessionMemoryMeasuredZeros(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment.CgroupMemory.Closing = CgroupMemoryClosing{
		CurrentBytes: 0,
		Composition:  &CgroupMemoryComposition{},
		LifetimePeak: &CgroupMemoryLifetimePeak{},
	}
	file.Environment.CgroupMemory.Events = &CgroupMemoryEvents{}
	file.Environment.MemoryPressure = &MemoryPressure{Scope: MemoryPressureCgroup}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, member := range []string{
		`"currentBytes":0`, `"anonBytes":0`, `"fileBytes":0`, `"shmemBytes":0`,
		`"lifetimePeak":{"bytes":0}`, `"low":0`, `"high":0`, `"max":0`,
		`"oom":0`, `"oomKill":0`, `"someStalledUs":0`, `"fullStalledUs":0`,
	} {
		if !bytesContain(data, member) {
			t.Errorf("measured zero %s was dropped from the wire: %s", member, data)
		}
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestCgroupV1MemoryKeepsOnlyExactFacts accepts current usage and the lifetime
// peak, while rejecting v2 composition and events. In particular, v1 failcnt
// is not an OOM/high event under a new spelling.
func TestCgroupV1MemoryKeepsOnlyExactFacts(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment.MemoryCapacity.CgroupLimit.Source = CgroupMemoryV1
	file.Environment.CgroupMemory = &CgroupMemory{
		Source: CgroupMemoryV1,
		Closing: CgroupMemoryClosing{
			CurrentBytes: 8 * 1024 * 1024 * 1024,
			LifetimePeak: &CgroupMemoryLifetimePeak{Bytes: 12 * 1024 * 1024 * 1024},
		},
	}
	file.Environment.MemoryPressure = &MemoryPressure{
		Scope: MemoryPressureHost, SomeStalledUs: 12000, FullStalledUs: 4000,
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("exact v1 fallback violations = %v, want none", violations)
	}

	file.Environment.CgroupMemory.Closing.Composition = &CgroupMemoryComposition{}
	file.Environment.CgroupMemory.Events = &CgroupMemoryEvents{}
	data, err = json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal v2-only facts: %v", err)
	}
	want := []Violation{
		{Code: ViolationUnexpectedField, Path: "environment.cgroupMemory.closing.composition"},
		{Code: ViolationUnexpectedField, Path: "environment.cgroupMemory.events"},
	}
	if got := ValidateDocument(DocumentSessionFile, data); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}

// TestMemoryCapacityPinsItsProvenance keeps effectiveBytes tied to the smaller
// measured stable bound and to the source that supplied it.
func TestMemoryCapacityPinsItsProvenance(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment.MemoryCapacity.EffectiveBytes = file.Environment.MemoryCapacity.PhysicalBytes
	file.Environment.MemoryCapacity.EffectiveSource = MemoryCapacityPhysical
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []Violation{{Code: ViolationCountMismatch, Path: "environment.memoryCapacity.effectiveBytes"}}
	if got := ValidateDocument(DocumentSessionFile, data); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}

func TestMemorySubsetsStayInternallyConsistent(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment.CgroupMemory.Closing.Composition = &CgroupMemoryComposition{
		AnonBytes: 1024, FileBytes: 128, ShmemBytes: 256,
	}
	file.Environment.MemoryPressure = &MemoryPressure{
		Scope: MemoryPressureCgroup, SomeStalledUs: 10, FullStalledUs: 11,
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []Violation{
		{Code: ViolationCountMismatch, Path: "environment.cgroupMemory.closing.composition.shmemBytes"},
		{Code: ViolationCountMismatch, Path: "environment.memoryPressure.fullStalledUs"},
	}
	if got := ValidateDocument(DocumentSessionFile, data); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}

// TestCgroupQuotaAbsenceMeansUnlimited: "max" in cpu.max is spelled by OMITTING
// quotaUs, and a zero is rejected — a ceiling of nothing is not a quota, and
// accepting it would let an unlimited cgroup masquerade as a fully-throttled
// one in every allocation figure derived from it.
func TestCgroupQuotaAbsenceMeansUnlimited(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment.CgroupCPU.QuotaUs = 0
	file.Run.CPU.AllocatedSource = CPUAllocationLogicalCPUs
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytesContain(data, "quotaUs") {
		t.Errorf("unlimited cgroup still emits quotaUs: %s", data)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}

	// The same document with an explicit zero is a violation, not an alternative
	// spelling of the same fact.
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	environment, _ := document["environment"].(map[string]any)
	cgroup, _ := environment["cgroupCpu"].(map[string]any)
	cgroup["quotaUs"] = 0
	zeroed, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal zeroed: %v", err)
	}
	want := []Violation{{Code: ViolationInvalidValue, Path: "environment.cgroupCpu.quotaUs"}}
	if got := ValidateDocument(DocumentSessionFile, zeroed); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}

// TestThrottleZeroIsAMeasurement is why the throttle counters live in a NESTED
// object: "the quota never throttled us" is a finding — it separates contention
// from quota starvation — and an omitempty member could not state it. Presence
// of the object carries "this was read"; the zeros inside carry the result.
func TestThrottleZeroIsAMeasurement(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment.CgroupCPU.Throttle = &CgroupThrottle{Periods: 35}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytesContain(data, "\"throttledPeriods\":0") {
		t.Errorf("a measured zero was dropped from the wire: %s", data)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestRunCPUBudgetTiesToTheRunWall: allocatedMs is allocatedMillicores over the
// run's OWN durationMs. Two numbers stated against different walls make every
// utilization ratio derived from them wrong, so the validator ties them.
func TestRunCPUBudgetTiesToTheRunWall(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Run.CPU.AllocatedMs = 28001
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []Violation{{Code: ViolationCountMismatch, Path: "run.cpu.allocatedMs"}}
	if got := ValidateDocument(DocumentSessionFile, data); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}

// TestRunCPUMayExceedItsAllocation is deliberate headroom, not an oversight: a
// cgroup quota is enforced PER PERIOD, so a run can burn more CPU than
// allocated × wall over short bursts. Clamping actualMs to the budget would
// hide exactly the oversubscription this block exists to expose.
func TestRunCPUMayExceedItsAllocation(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Run.CPU.ActualMs = file.Run.CPU.AllocatedMs + 5000
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none — over-allocation is measurable, not invalid", violations)
	}
}

// TestHostCPUSharesStayWithinTheirWindow: steal and iowait are COLUMNS of the
// /proc/stat total over the same window. A producer that subtracted two
// mismatched samples would publish a steal share above 100%, which is a
// producer bug the contract catches rather than a runner fact.
func TestHostCPUSharesStayWithinTheirWindow(t *testing.T) {
	file := sessionFileWithEnvironment()
	file.Environment.HostCPU = &HostCPUTime{StealTicks: 900, IOWaitTicks: 500, TotalTicks: 404}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []Violation{
		{Code: ViolationCountMismatch, Path: "environment.hostCpu.ioWaitTicks"},
		{Code: ViolationCountMismatch, Path: "environment.hostCpu.stealTicks"},
	}
	if got := ValidateDocument(DocumentSessionFile, data); !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateDocument = %v, want %v", got, want)
	}
}
