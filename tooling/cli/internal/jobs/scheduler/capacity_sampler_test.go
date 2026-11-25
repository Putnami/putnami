package scheduler

import (
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestCapacitySampler_SamplesTheGrantedCPUsAndTheLoad(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-the-cpu-it-grants-and-the-machine-load", "the-capacity-report-samples-the-granted-cpus-and-the-load")
	clock := time.Unix(1000, 0)
	sampler := newCapacitySampler(4, 4)
	sampler.started = clock
	sampler.now = func() time.Time { return clock }
	sampler.load = func() (float64, bool) { return 2.5, true }

	sampler.sample()
	sampler.grant(3)
	sampler.grant(1)
	clock = clock.Add(5 * time.Second)
	sampler.sample()
	sampler.release(3)
	sampler.release(1)
	report := sampler.finish()

	if report.CPUs != 4 || report.Workers != 4 || report.PeakGrantedCPU != 4 {
		t.Fatalf("report = %+v, want 4 cpus, 4 workers and a peak of 4", report)
	}
	if len(report.Samples) != 3 {
		t.Fatalf("samples = %+v, want the start, one tick and the end", report.Samples)
	}
	if got := report.Samples[1]; got.At != 5*time.Second || got.GrantedCPU != 4 || !got.LoadKnown || got.Load != 2.5 {
		t.Errorf("tick sample = %+v", got)
	}
	if got := report.Samples[2]; got.GrantedCPU != 0 {
		t.Errorf("end sample = %+v, want nothing granted", got)
	}
}

func TestCapacitySampler_HalvesItsSamplesWhenFull(t *testing.T) {
	t.Parallel()
	clock := time.Unix(0, 0)
	sampler := newCapacitySampler(2, 2)
	sampler.started = clock
	sampler.now = func() time.Time { return clock }
	sampler.load = func() (float64, bool) { return 0, false }
	for range maxCapacitySamples + 1 {
		sampler.sample()
		clock = clock.Add(capacitySampleInterval)
	}
	report := sampler.finish()
	if report.Interval != 2*capacitySampleInterval {
		t.Fatalf("interval = %s, want it doubled", report.Interval)
	}
	if len(report.Samples) != maxCapacitySamples/2+2 {
		t.Fatalf("samples = %d, want every other one kept, then two more", len(report.Samples))
	}
	if report.Samples[1].At != 2*capacitySampleInterval {
		t.Errorf("second kept sample at %s, want every other one", report.Samples[1].At)
	}
}

func TestCapacitySampler_NilAndUnstartedRecordNothingAndNeverBlock(t *testing.T) {
	t.Parallel()
	var none *capacitySampler
	none.grant(1)
	none.release(1)
	none.start()
	if none.finish() != nil {
		t.Fatal("a nil sampler reported")
	}
	unstarted := newCapacitySampler(1, 1)
	if report := unstarted.finish(); report == nil || len(report.Samples) != 1 {
		t.Fatalf("unstarted report = %+v, want its one closing sample", report)
	}
}

func TestCapacitySampler_SamplesOnEveryTickOfItsSpacing(t *testing.T) {
	t.Parallel()
	sampler := newCapacitySampler(1, 1)
	for tick := 1; tick <= 4; tick++ {
		if !sampler.due(tick) {
			t.Fatalf("tick %d skipped at the base spacing", tick)
		}
	}
	sampler.interval = 4 * capacitySampleInterval
	var due []int
	for tick := 1; tick <= 8; tick++ {
		if sampler.due(tick) {
			due = append(due, tick)
		}
	}
	if len(due) != 2 || due[0] != 4 || due[1] != 8 {
		t.Fatalf("due ticks = %v, want every fourth", due)
	}
}
