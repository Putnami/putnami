package machine

import (
	"math"
	"slices"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
)

// orchestrationSummary projects a run's settled orchestration report onto
// run.orchestration. tasks is the run's counts.total, the denominator of
// perTaskMs. A run that recorded no report has none.
func orchestrationSummary(snapshot *jobs.OrchestrationSnapshot, tasks int) *protocolcli.RunOrchestration {
	if snapshot == nil {
		return nil
	}
	out := &protocolcli.RunOrchestration{}
	for _, phase := range protocolcli.OrchestrationPhases {
		for _, recorded := range snapshot.Phases {
			if recorded.Phase == phase {
				out.Phases = append(out.Phases, protocolcli.OrchestrationPhase{Phase: phase, WallMs: millis(recorded.Wall)})
			}
		}
	}
	for _, origin := range []struct {
		name   string
		ledger jobs.ProcessLedger
	}{{protocolcli.OrchestrationOriginEngine, snapshot.Engine}, {protocolcli.OrchestrationOriginJobs, snapshot.Jobs}} {
		for _, kind := range protocolcli.OrchestrationKinds {
			count := origin.ledger.Of(kind)
			if count.Starts <= 0 {
				continue
			}
			entry := protocolcli.OrchestrationProcesses{Origin: origin.name, Kind: kind, Count: count.Starts}
			if count.Measured > 0 {
				ms := millis(count.CPU)
				entry.CPUMs = &ms
			}
			out.Processes = append(out.Processes, entry)
		}
		subcommands := make([]string, 0, len(origin.ledger.Git))
		for subcommand, calls := range origin.ledger.Git {
			if subcommand != "" && calls > 0 {
				subcommands = append(subcommands, subcommand)
			}
		}
		slices.Sort(subcommands)
		for _, subcommand := range subcommands {
			out.Git = append(out.Git, protocolcli.OrchestrationGitCalls{
				Origin: origin.name, Subcommand: subcommand, Count: origin.ledger.Git[subcommand],
			})
		}
	}
	if snapshot.CPUMeasured {
		engine, extension, control, cli, git := snapshot.OrchestrationCPU()
		cpu := &protocolcli.OrchestrationCPU{
			EngineMs:    millis(engine),
			ExtensionMs: millis(extension),
			ControlMs:   millis(control),
			CLIMs:       millis(cli),
			GitMs:       millis(git),
			RunMs:       millis(snapshot.RunCPU),
		}
		// The total is the sum of the rounded parts, so a reader adding them
		// finds it exactly.
		cpu.TotalMs = cpu.EngineMs + cpu.ExtensionMs + cpu.ControlMs + cpu.CLIMs + cpu.GitMs
		if cpu.RunMs > 0 {
			share := cpu.TotalMs * 1000 / cpu.RunMs
			cpu.SharePermille = &share
		}
		if tasks > 0 {
			perTask := cpu.TotalMs / int64(tasks)
			cpu.PerTaskMs = &perTask
		}
		cpu.UnreportedProcesses = nonNegative64(snapshot.UnreportedProcesses)
		out.CPU = cpu
	}
	out.Capacity = capacitySummary(snapshot.Capacity)
	if len(out.Phases) == 0 && len(out.Processes) == 0 && len(out.Git) == 0 && out.CPU == nil && out.Capacity == nil {
		return nil
	}
	return out
}

// taskOrchestration projects a task's waits onto its record: each step the
// task entered, even under a millisecond, and nothing when it entered none.
func taskOrchestration(waits jobs.TaskWaits) *protocolcli.TaskOrchestration {
	if !waits.Any() {
		return nil
	}
	ms := func(observed bool, d time.Duration) *int64 {
		if !observed {
			return nil
		}
		value := millis(d)
		return &value
	}
	return &protocolcli.TaskOrchestration{
		KeyMs:      ms(waits.KeyObserved, waits.Key),
		LockWaitMs: ms(waits.LockObserved, waits.LockWait),
		RestoreMs:  ms(waits.RestoreObserved, waits.Restore),
	}
}

// capacitySummary projects the scheduler's capacity samples onto
// run.orchestration.capacity. A report with no sample has none.
func capacitySummary(report *jobs.CapacityReport) *protocolcli.RunCapacity {
	if report == nil || len(report.Samples) == 0 {
		return nil
	}
	out := &protocolcli.RunCapacity{
		CPUs:            max(report.CPUs, 1),
		Workers:         max(report.Workers, 1),
		IntervalMs:      max(millis(report.Interval), 1),
		PeakGrantedCPUs: max(report.PeakGrantedCPU, 0),
		Samples:         make([]protocolcli.CapacitySample, 0, len(report.Samples)),
	}
	for _, sample := range report.Samples {
		entry := protocolcli.CapacitySample{AtMs: max(millis(sample.At), 0), GrantedCPUs: max(sample.GrantedCPU, 0)}
		out.PeakGrantedCPUs = max(out.PeakGrantedCPUs, entry.GrantedCPUs)
		if sample.LoadKnown && sample.Load >= 0 && !math.IsNaN(sample.Load) && !math.IsInf(sample.Load, 0) {
			load := int64(math.Round(sample.Load * 100))
			entry.LoadCenti = &load
			if out.PeakLoadCenti == nil || load > *out.PeakLoadCenti {
				peak := load
				out.PeakLoadCenti = &peak
			}
		}
		out.Samples = append(out.Samples, entry)
	}
	return out
}
