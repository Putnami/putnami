package jobs

import "time"

// CgroupThrottle is cgroup v2 CPU bandwidth accounting over the session window.
// Zero values here are MEASUREMENTS ("the quota never suspended us"), which is
// why they travel in their own struct: the projection publishes the object or
// omits it whole, never a zero that could be either.
type CgroupThrottle struct {
	Periods          int64
	ThrottledPeriods int64
	ThrottledUs      int64
}

// CgroupCPU is the cgroup CPU controller as this session saw it.
type CgroupCPU struct {
	// PeriodUs is the bandwidth period from cgroup v2's cpu.max or cgroup
	// v1's cpu.cfs_period_us.
	PeriodUs int64
	// QuotaUs is the CPU time allowed per period, or 0 when the controller is
	// unlimited. Zero therefore means UNLIMITED, not "a quota of nothing", and
	// the projection omits it.
	QuotaUs int64
	// UsageUs is the whole cgroup's CPU over the window, from cgroup v2's
	// cpu.stat usage_usec or cgroup v1's cpuacct.usage. It is wider than the
	// execution ledger's total because it includes the CLI process itself.
	UsageUs int64
	// HasUsage separates "the controller exposed no usage counter" from a
	// genuine zero.
	HasUsage bool
	// Throttle is nil when cpu.stat exposed no bandwidth counters.
	Throttle *CgroupThrottle
}

// CPUPressure is PSI for CPU over the session window: the microseconds at least
// one task spent stalled waiting for a CPU. It is the shared-host signal — a
// runner well inside its own quota can still be slow because the host is
// oversubscribed.
type CPUPressure struct {
	SomeStalledUs int64
}

// HostCPUTime is the /proc/stat aggregate over the session window, in USER_HZ
// ticks exactly as the kernel states them. The ticks are NOT converted to
// milliseconds: that needs a USER_HZ this process cannot read, and Total is
// published beside them so the only reading this block is for — the share of
// the window lost to steal or iowait — is unit-free.
type HostCPUTime struct {
	StealTicks  int64
	IOWaitTicks int64
	TotalTicks  int64
}

const (
	MemoryCapacityPhysical    = "physical"
	MemoryCapacityCgroupLimit = "cgroup-limit"
	CgroupMemoryV1            = "cgroup-v1"
	CgroupMemoryV2            = "cgroup-v2"
	MemoryPressureHost        = "host"
	MemoryPressureCgroup      = "cgroup"
)

// CgroupMemoryLimit is a finite memory-controller limit and the controller
// layout that supplied it. An unlimited controller is represented by absence.
type CgroupMemoryLimit struct {
	Bytes  int64
	Source string
}

// MemoryCapacity is the stable memory bound used for scheduler admission.
// PhysicalBytes and CgroupLimit are independent measurements; EffectiveBytes
// is their smaller available value and EffectiveSource names that input.
type MemoryCapacity struct {
	PhysicalBytes   int64
	CgroupLimit     *CgroupMemoryLimit
	EffectiveBytes  int64
	EffectiveSource string
}

// CgroupMemoryComposition is the complete closing cgroup v2 memory.stat
// subset. FileBytes includes ShmemBytes and the two must not be summed.
type CgroupMemoryComposition struct {
	AnonBytes  int64
	FileBytes  int64
	ShmemBytes int64
}

// CgroupMemoryLifetimePeak is the cgroup-lifetime peak sampled at close.
type CgroupMemoryLifetimePeak struct {
	Bytes int64
}

// CgroupMemoryClosing contains closing gauges, never session deltas.
type CgroupMemoryClosing struct {
	CurrentBytes int64
	Composition  *CgroupMemoryComposition
	LifetimePeak *CgroupMemoryLifetimePeak
}

// CgroupMemoryEvents is the complete cgroup v2 memory.events subset windowed
// to this session.
type CgroupMemoryEvents struct {
	Low     int64
	High    int64
	Max     int64
	OOM     int64
	OOMKill int64
}

// CgroupMemory carries exact controller facts for one stable cgroup identity.
type CgroupMemory struct {
	Source  string
	Closing CgroupMemoryClosing
	Events  *CgroupMemoryEvents
}

// MemoryPressure is a complete host- or cgroup-scoped memory PSI delta.
type MemoryPressure struct {
	Scope         string
	SomeStalledUs int64
	FullStalledUs int64
}

// RunnerEnvironment is one session's view of the machine it ran on. Optional
// pointer members are absent exactly when the platform could not measure them.
type RunnerEnvironment struct {
	OS          string
	Arch        string
	LogicalCPUs int
	CPUModel    string
	// Window is the wall between the opening and closing samples — the window
	// every delta below covers. It is measured from the samples themselves, not
	// taken from the run's duration, so the two cannot drift.
	Window         time.Duration
	Cgroup         *CgroupCPU
	CPUPressure    *CPUPressure
	HostCPU        *HostCPUTime
	MemoryCapacity *MemoryCapacity
	CgroupMemory   *CgroupMemory
	MemoryPressure *MemoryPressure
}

// AllocatedMillicores is the CPU this runner may use, in thousandths of a core,
// and whether that number came from a quota or merely from the visible core
// count. The two are different physics: a quota SUSPENDS a run that exceeds it,
// while a core count is only the point past which there is nothing left to run
// on, so a consumer that conflated them would read throttling into a machine
// that was simply busy.
//
// An unlimited cgroup falls back to the core count, which is the honest answer:
// nothing bounds the run below what it can see.
func (e *RunnerEnvironment) AllocatedMillicores() (millicores int, fromQuota bool) {
	if e == nil {
		return 0, false
	}
	if e.Cgroup != nil && e.Cgroup.QuotaUs > 0 && e.Cgroup.PeriodUs > 0 {
		quota := int(e.Cgroup.QuotaUs * 1000 / e.Cgroup.PeriodUs)
		if quota > 0 {
			return quota, true
		}
	}
	if e.LogicalCPUs > 0 {
		return e.LogicalCPUs * 1000, false
	}
	return 0, false
}
