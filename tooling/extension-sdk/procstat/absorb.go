package procstat

import (
	"sync"
	"time"

	runtimeproto "go.putnami.dev/protocol/runtime"
)

// reported sums the reports of the job processes this process started.
var reported struct {
	mu         sync.Mutex
	kinds      map[Kind]*KindCount
	git        map[string]int64
	selfCPU    time.Duration
	reports    int64
	unreported int64
}

// Absorb adds the report a job process wrote in its result to the reported
// ledger. A nil report counts a job process that reported nothing, a runtime
// not built on this SDK, and one without selfCpuMs counts as unmeasured.
func Absorb(report *runtimeproto.ProcessReport) {
	reported.mu.Lock()
	defer reported.mu.Unlock()
	if report == nil || report.SelfCPUMs == nil {
		reported.unreported++
	} else {
		reported.reports++
		reported.selfCPU += time.Duration(*report.SelfCPUMs) * time.Millisecond
	}
	if report == nil {
		return
	}
	if reported.kinds == nil {
		reported.kinds = map[Kind]*KindCount{}
		reported.git = map[string]int64{}
	}
	for _, start := range report.Starts {
		kind := Kind(start.Kind)
		entry := reported.kinds[kind]
		if entry == nil {
			entry = &KindCount{Kind: kind}
			reported.kinds[kind] = entry
		}
		entry.Starts += start.Count
		if start.CPUMs != nil {
			entry.CPU += time.Duration(*start.CPUMs) * time.Millisecond
			entry.Measured++
		}
	}
	for _, call := range report.Git {
		reported.git[call.Subcommand] += call.Count
	}
}

// Reported is the sum of the job-process reports Absorb received.
type Reported struct {
	// Kinds has one entry per kind, in Kinds order, started or not.
	Kinds []KindCount
	// Git maps a subcommand to the calls job processes reported.
	Git map[string]int64
	// SelfCPU is the CPU job processes reported for themselves.
	SelfCPU time.Duration
	// Reports counts the job processes that reported their own CPU, and
	// Unreported those that did not.
	Reports, Unreported int64
}

// TakeReported reads the reported ledger.
func TakeReported() Reported {
	reported.mu.Lock()
	defer reported.mu.Unlock()
	out := Reported{
		Kinds:      make([]KindCount, len(Kinds)),
		Git:        make(map[string]int64, len(reported.git)),
		SelfCPU:    reported.selfCPU,
		Reports:    reported.reports,
		Unreported: reported.unreported,
	}
	for i, kind := range Kinds {
		out.Kinds[i] = KindCount{Kind: kind}
		if entry := reported.kinds[kind]; entry != nil {
			out.Kinds[i] = *entry
		}
	}
	for subcommand, calls := range reported.git {
		out.Git[subcommand] = calls
	}
	return out
}

// Of returns the entry of kind.
func (r Reported) Of(kind Kind) KindCount {
	for _, entry := range r.Kinds {
		if entry.Kind == kind {
			return entry
		}
	}
	return KindCount{Kind: kind}
}
