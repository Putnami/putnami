package clicore

import (
	"errors"
	"strings"
	"testing"
)

func TestWorstStatusOrdersTheVocabulary(t *testing.T) {
	for _, tc := range []struct {
		states []StatusState
		want   StatusState
	}{
		{nil, StatusOK},
		{[]StatusState{StatusOK, StatusOK}, StatusOK},
		{[]StatusState{StatusOK, StatusDegraded}, StatusDegraded},
		{[]StatusState{StatusDegraded, StatusUnknown}, StatusUnknown},
		{[]StatusState{StatusFailing, StatusUnknown, StatusOK}, StatusFailing},
		{[]StatusState{StatusOK, "pending"}, StatusUnknown},
	} {
		if got := WorstStatus(tc.states...); got != tc.want {
			t.Errorf("WorstStatus(%v) = %s, want %s", tc.states, got, tc.want)
		}
	}
	if ValidStatusState("pending") || !ValidStatusState(StatusDegraded) {
		t.Fatal("ValidStatusState does not match the closed vocabulary")
	}
}

func TestRollUpReadsEveryDescendant(t *testing.T) {
	node := StatusNode{ID: "env", State: StatusOK, Children: []StatusNode{
		{ID: "env.a", State: StatusOK},
		{ID: "env.b", State: StatusOK, Children: []StatusNode{{ID: "env.b.1", State: StatusFailing}}},
	}}
	if got := node.RollUp(); got != StatusFailing {
		t.Fatalf("RollUp = %s, want failing from a grandchild", got)
	}
}

func TestUnknownStatusKeepsTheFirstLineOfTheError(t *testing.T) {
	node := UnknownStatus("ci", "ci", errors.New("  control plane timed out\nretry later  "), "putnami cloud ci status")
	if node.State != StatusUnknown || node.Detail != "control plane timed out" || node.Fix != "putnami cloud ci status" {
		t.Fatalf("node = %+v", node)
	}
	if got := UnknownStatus("ci", "ci", nil, "").Detail; got != "no answer" {
		t.Fatalf("detail without an error = %q, want no answer", got)
	}
}

func TestRenderStatusTableAlignsAndNamesTheNextCommand(t *testing.T) {
	more := func(node StatusNode) string { return "putnami cloud " + node.ID + " status" }
	lines := RenderStatusTable([]StatusNode{
		{ID: "ci", Title: "ci", State: StatusOK, Detail: "subscribed", Fix: "putnami cloud ci enable"},
		{ID: "registries", Title: "registries", State: StatusFailing, Detail: "npm token expired", Fix: "putnami cloud registries setup"},
		{ID: "cache", Title: "cache", State: StatusDegraded, Detail: "disabled"},
		{ID: "odd", Title: "odd", State: "pending"},
	}, more)
	want := []string{
		"STATE     ENTRY       DETAIL             NEXT",
		"ok        ci          subscribed         putnami cloud ci status",
		"failing   registries  npm token expired  putnami cloud registries setup",
		"degraded  cache       disabled           putnami cloud cache status",
		"unknown   odd                            putnami cloud odd status",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("table =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	bare := RenderStatusTable([]StatusNode{{Title: "env", State: StatusOK, Detail: "prod ready"}}, nil)
	if len(bare) != 2 || bare[1] != "ok     env    prod ready" {
		t.Fatalf("table without commands = %q", bare)
	}
}

func TestRenderStatusReportPrintsHeaderMetricsAndChecks(t *testing.T) {
	node := StatusNode{
		ID: "ci", Title: "ci", State: StatusUnknown, Detail: "runner provenance unknown",
		Metrics: []StatusMetric{
			CountMetric("queued_runs", "queued runs", 0, MetricHealth),
			CountMetric("reused_tasks", "reused tasks", 311, MetricHealth).Of(645).Over("last run"),
			{ID: "spend", Title: "spend", Value: 12.3, Unit: UnitEUR, Window: "7 days", Kind: MetricUsage, Estimated: true},
		},
		Children: []StatusNode{
			{ID: "ci.pipeline", Title: "pipeline", State: StatusOK, Detail: "healthy", Fix: "never shown"},
			{ID: "ci.runner", Title: "runner", State: StatusUnknown, Detail: "a newer run is pending", Fix: "wait for the newest run",
				Children: []StatusNode{{ID: "ci.runner.image", Title: "image", State: StatusOK, Detail: "sha256:d405"}}},
		},
		Fix: "putnami cloud ci status",
	}
	want := []string{
		"ci  unknown  runner provenance unknown",
		"",
		"  METRIC        VALUE             WINDOW",
		"  queued runs   0                 now",
		"  reused tasks  311 of 645 (48%)  last run",
		"  spend         ~€12.30           7 days",
		"",
		"  STATE    CHECK     DETAIL",
		"  ok       pipeline  healthy",
		"  unknown  runner    a newer run is pending",
		"                     fix: wait for the newest run",
		"  ok         image   sha256:d405",
	}
	if got := RenderStatusReport(node); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("report =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	bare := RenderStatusReport(StatusNode{Title: "cache", State: StatusDegraded, Detail: "not configured", Fix: "putnami cloud setup"})
	if strings.Join(bare, "\n") != "cache  degraded  not configured\n\nnext: putnami cloud setup" {
		t.Fatalf("report without checks = %q", bare)
	}
}

func TestStatusMetricFormatsEachUnit(t *testing.T) {
	for _, tc := range []struct {
		metric StatusMetric
		want   string
	}{
		{CountMetric("n", "n", 1204331, MetricUsage), "1,204,331"},
		{StatusMetric{Value: 212 << 30, Unit: UnitBytes}.Of(300 << 30), "212 GiB of 300 GiB (71%)"},
		{StatusMetric{Value: 1536, Unit: UnitBytes}, "1.5 KiB"},
		{StatusMetric{Value: 512, Unit: UnitBytes}, "512 B"},
		{StatusMetric{Value: 48.04, Unit: UnitPercent}, "48%"},
		{StatusMetric{Value: 3075, Unit: UnitSeconds}, "51m15s"},
		{StatusMetric{Value: 4.5, Unit: UnitEUR}, "€4.50"},
	} {
		if got := tc.metric.FormatValue(); got != tc.want {
			t.Errorf("FormatValue(%+v) = %q, want %q", tc.metric, got, tc.want)
		}
	}
}

func TestWriteStatusExitsOnFailingOrStrict(t *testing.T) {
	var out []string
	ioctx := IO{Stdout: func(line string) { out = append(out, line) }}
	ok := StatusNode{ID: "cache", Title: "cache", State: StatusDegraded, Detail: "not configured"}
	if err := WriteStatus(map[string]any{}, ioctx, ok); err != nil {
		t.Fatalf("degraded without --strict failed: %v", err)
	}
	if err := WriteStatus(map[string]any{"strict": true}, ioctx, ok); err == nil || ExitCode(err) != ExitFailure {
		t.Fatalf("degraded with --strict = %v, want exit %d", err, ExitFailure)
	}
	failing := StatusNode{ID: "secrets", Title: "secrets", State: StatusFailing, Detail: "2 required secrets missing"}
	err := WriteStatus(map[string]any{}, ioctx, failing)
	if err == nil || ExitCode(err) != ExitFailure || err.Error() != "secrets is failing: 2 required secrets missing" {
		t.Fatalf("failing = %v", err)
	}
	if data, isNode := resultData(err).(StatusNode); !isNode || data.ID != "secrets" {
		t.Fatalf("failure carries %#v, want the node", resultData(err))
	}
	if len(out) != 3 || !strings.HasPrefix(out[2], "secrets  failing") {
		t.Fatalf("text output = %q", out)
	}
	if err := WriteStatusSynthesis(map[string]any{}, ioctx, []StatusNode{ok, failing}, nil); err == nil || err.Error() != "workspace is failing: cache degraded, secrets failing" {
		t.Fatalf("synthesis = %v", err)
	}
}
