package specgate

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestCollectReadsSaysWhetherAVerdictReadsTheClock pins what a machine worker
// holds a gate under (CLI ADR 0116 §6a): a group whose criteria are
// acceptance checks or invocation thresholds is decided the same at any
// instant, while a threshold over a rolling window goes stale with time, so
// its verdict reads the clock; a group under off evaluates nothing.
func TestCollectReadsSaysWhetherAVerdictReadsTheClock(t *testing.T) {
	// A rolling objective is a live-verified requirement's.
	rolling := strings.Replace(strings.Replace(fixtureProjection, `"id": "issue",
          "stage": "coded",`, `"id": "issue",
          "stage": "live-verified",`, 1),
		`"verification": {"kind": "acceptance", "checks": ["issue-check"]}`,
		`"verification": {"kind": "threshold", "checks": ["issue-check"], "metric": "billing.delivery.success",
		  "aggregation": "ratio", "operator": "gte", "target": 0.999, "unit": "ratio",
		  "window": {"kind": "rolling", "seconds": 3600}, "environment": "production", "maxAgeSeconds": 3600}`, 1)
	if rolling == fixtureProjection {
		t.Fatal("the fixture's acceptance criterion was not found")
	}
	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		projection, mode string
		clock            bool
	}{
		"an acceptance check":            {fixtureProjection, "", false},
		"a threshold over a rolling one": {rolling, "", true},
		"a rolling one under off":        {rolling, "off", false},
	} {
		fixture := newGateFixture(t, c.projection, fixtureReport("passed"))
		if c.mode != "" {
			fixture.setPolicy("project", c.mode)
		}
		record, reads, err := CollectReads(fixture.ws, fixture.planned, fixture.results, at, nil, fixture.producers)
		if err != nil || record == nil {
			t.Fatalf("%s: collect: %v, %v", name, record, err)
		}
		if len(record.Groups) != 1 || record.Groups[0].Feature != "billing/invoicing" {
			t.Fatalf("%s: groups %+v, want the projection's one group read", name, record.Groups)
		}
		if reads.Clock != c.clock {
			t.Errorf("%s: reads the clock %v, want %v", name, reads.Clock, c.clock)
		}
		if again, err := Collect(fixture.ws, fixture.planned, fixture.results, at, nil, fixture.producers); err != nil || !reflect.DeepEqual(again, record) {
			t.Errorf("%s: Collect gave %+v, %v, want what CollectReads collected", name, again, err)
		}
	}
}
