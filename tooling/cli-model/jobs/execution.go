package jobs

import "time"

// Execution is ONE subprocess the CLI spawned, and what that process tree
// actually cost the machine. Every member is MEASURED: a counter the platform
// does not expose stays zero and is omitted downstream rather than being
// derived from something else.
type Execution struct {
	// ID is the stable handle every logical record produced by this execution
	// references.
	ID string
	// Wall is the subprocess's own wall time: immediately before fork/exec until
	// wait returned. It deliberately excludes the CLI-side setup that
	// JobResult.Duration covers, so it pairs with the rusage below, which
	// accounts for the child only.
	Wall time.Duration
	// UserCPU and SystemCPU are the process tree's CPU time, waited children
	// included.
	UserCPU   time.Duration
	SystemCPU time.Duration
	// MaxRSSBytes is peak resident set size, normalized to BYTES by the
	// platform-specific reader (rusage_darwin.go / rusage_linux.go) because
	// ru_maxrss is bytes on darwin and kilobytes on linux. Zero where the
	// platform reports no peak.
	MaxRSSBytes int64
	// IOInBlocks and IOOutBlocks are the rusage block-IO operation counts. Zero
	// where the platform does not account them, which includes darwin for most
	// processes — an absent counter, not a measured zero.
	IOInBlocks  int64
	IOOutBlocks int64
	// Concurrency is the tool-native parallelism this execution was granted: the
	// CPU budget exported to the subprocess as PUTNAMI_CPU_BUDGET (and GOMAXPROCS
	// for Go tooling). Zero when the scheduler granted none, so CPU time is never
	// read against a bound that was never stated.
	Concurrency int
	// BatchSize is profile-only context: batch RSS is one physical high-water
	// mark, not one independent peak per logical member. It is deliberately not
	// exported as execution telemetry because the public execution record already
	// exposes its logical fan-out as Tasks.
	BatchSize int
}

// CPUTime is the execution's total CPU: user plus system.
func (e *Execution) CPUTime() time.Duration {
	if e == nil {
		return 0
	}
	return e.UserCPU + e.SystemCPU
}
