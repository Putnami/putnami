package parallel

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"testing"
	"time"
)

// TestMapBoundedSustainsItsThroughputFloor is the invocation-window KPI of the
// dispatch path. It is deliberately NOT a service objective: the authored floor
// sits roughly thirty times below the rate the loop actually reaches, so what
// it catches is a structural regression — a lock added to the hot path, a
// per-item allocation, a goroutine per item where the limit said otherwise —
// and never a busy runner.
//
// The measurement is published through ObserveMeasurement, which states the
// observed rate and nothing else; the verdict is recomputed by the gate from
// the target authored in putnami.features.json. A test cannot pass its own KPI.
func TestMapBoundedSustainsItsThroughputFloor(t *testing.T) {
	const items = 20000
	const limit = 8

	in := make([]int, items)
	for i := range in {
		in[i] = i
	}

	start := time.Now()
	out, err := MapBounded(context.Background(), in, limit, func(_ context.Context, item int) (int, error) {
		return item, nil
	})
	elapsed := time.Since(start)

	// The rate is only meaningful if the run actually did the work, so the
	// correctness assertions come first and a failed run publishes nothing.
	if err != nil {
		t.Fatalf("MapBounded over %d items: %v", items, err)
	}
	if len(out) != items {
		t.Fatalf("MapBounded returned %d results, want %d", len(out), items)
	}
	for i, got := range out {
		if got != i {
			t.Fatalf("result[%d] = %d, want %d", i, got, i)
		}
	}
	if elapsed <= 0 {
		t.Fatalf("elapsed = %v, want a positive duration to divide by", elapsed)
	}

	spectest.ObserveMeasurement(t, "go/bounded-parallel-work", "dispatch-throughput-budget",
		"bounded-dispatch-sustains-the-throughput-floor", spectest.Measurement{
			Name:        "dispatch.throughput",
			Aggregation: "value",
			Value:       float64(items) / elapsed.Seconds(),
			Unit:        "items-per-second",
		})
}
