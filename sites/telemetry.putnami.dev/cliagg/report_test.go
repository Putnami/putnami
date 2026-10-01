package cliagg

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	stderrors "errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixedNow is the clock every report test resolves its window against:
// 2026-07-28T15:04:05Z. The 7d window is therefore 2026-07-22 .. 2026-07-29.
var fixedNow = time.Date(2026, 7, 28, 15, 4, 5, 0, time.UTC)

func window(t *testing.T, spec string) Window {
	t.Helper()
	w, err := ParseWindow(spec, fixedNow)
	if err != nil {
		t.Fatalf("ParseWindow(%q): %v", spec, err)
	}
	return w
}

func TestParseWindowAcceptsOnlyFixedCalendarWindows(t *testing.T) {
	tests := []struct {
		spec            string
		wantStart       string
		wantEnd         string
		wantDays        int
		wantCompleteThr string
	}{
		{spec: Window1d, wantStart: "2026-07-28", wantEnd: "2026-07-29", wantDays: 1, wantCompleteThr: ""},
		{spec: Window7d, wantStart: "2026-07-22", wantEnd: "2026-07-29", wantDays: 7, wantCompleteThr: "2026-07-27"},
		{spec: Window30d, wantStart: "2026-06-29", wantEnd: "2026-07-29", wantDays: 30, wantCompleteThr: "2026-07-27"},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			w, err := ParseWindow(tc.spec, fixedNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w.Start != tc.wantStart || w.End != tc.wantEnd {
				t.Errorf("bounds = [%s,%s), want [%s,%s)", w.Start, w.End, tc.wantStart, tc.wantEnd)
			}
			if w.Days != tc.wantDays {
				t.Errorf("days = %d, want %d", w.Days, tc.wantDays)
			}
			if !w.PartialBucket {
				t.Error("partialBucket = false, want true: the last bucket is the current UTC day")
			}
			if w.CompleteThrough != tc.wantCompleteThr {
				t.Errorf("completeThrough = %q, want %q", w.CompleteThrough, tc.wantCompleteThr)
			}
			if days := w.days(); len(days) != tc.wantDays || days[len(days)-1] != "2026-07-28" {
				t.Errorf("days() = %v, want %d buckets ending 2026-07-28", days, tc.wantDays)
			}
		})
	}
}

func TestParseWindowRejectsEverythingElse(t *testing.T) {
	rejected := []string{
		"", " ", "1D", "7D", "2d", "0d", "31d", "365d", "-1d", "1h", "7", "all", "custom",
		"1d;DROP TABLE cli_device_day", "2026-07-01..2026-07-28", "month", "1d,7d",
	}
	for _, spec := range rejected {
		t.Run(strings.ReplaceAll(spec, " ", "_"), func(t *testing.T) {
			if _, err := ParseWindow(spec, fixedNow); !stderrors.Is(err, ErrUnsupportedWindow) {
				t.Fatalf("ParseWindow(%q) error = %v, want ErrUnsupportedWindow", spec, err)
			}
		})
	}
}

// TestParseWindowUsesUTCCalendar pins the window to the UTC calendar day
// regardless of the caller's location, so bounds are reproducible.
func TestParseWindowUsesUTCCalendar(t *testing.T) {
	// 2026-07-28T23:30 in UTC+2 is 2026-07-28T21:30Z: still the 28th in UTC.
	east := time.FixedZone("east", 2*60*60)
	w, err := ParseWindow(Window1d, time.Date(2026, 7, 28, 23, 30, 0, 0, east))
	if err != nil {
		t.Fatal(err)
	}
	if w.Start != "2026-07-28" || w.End != "2026-07-29" {
		t.Fatalf("bounds = [%s,%s), want [2026-07-28,2026-07-29)", w.Start, w.End)
	}
}

// healthyRaw is a fixture with every cohort comfortably above the threshold, so
// nothing is suppressed and the whole contract is visible.
func healthyRaw() raw {
	return raw{
		devices: 42,
		dailyDevices: map[string]int64{
			"2026-07-26": 10, "2026-07-27": 20, "2026-07-28": 12,
		},
		dailySessions: map[string]int64{
			"2026-07-26": 30, "2026-07-27": 40, "2026-07-28": 30,
		},
		counters: map[string]map[string]int64{
			DimCommand:     {"build": 60, "test": 25, "lint": 15},
			DimOutcome:     {"success": 80, "error:api": 20},
			DimCLIVersion:  {"1.2.3": 60, "1.3.0": 40},
			DimOS:          {"darwin": 55, "linux": 45},
			DimArch:        {"arm64": 70, "amd64": 30},
			DimInteractive: {"true": 60, "false": 40},
		},
	}
}

func TestBuildReportPublishesSummaryDailyAndBreakdowns(t *testing.T) {
	w := window(t, Window7d)
	report := buildReport(w, healthyRaw(), fixedNow)

	if report.Window != w {
		t.Errorf("window echo = %+v, want %+v", report.Window, w)
	}
	if report.SuppressionThreshold != SuppressionThreshold {
		t.Errorf("suppressionThreshold = %d, want %d", report.SuppressionThreshold, SuppressionThreshold)
	}
	if got := mustValue(t, report.Summary.Devices); got != 42 {
		t.Errorf("summary.devices = %d, want 42", got)
	}
	if got := mustValue(t, report.Summary.Sessions); got != 100 {
		t.Errorf("summary.sessions = %d, want 100 (sum of the outcome dimension)", got)
	}
	if got := mustValue(t, report.Summary.Commands); got != 100 {
		t.Errorf("summary.commands = %d, want 100", got)
	}

	if len(report.Daily) != 7 {
		t.Fatalf("daily points = %d, want 7", len(report.Daily))
	}
	if report.Daily[0].Day != "2026-07-22" || report.Daily[6].Day != "2026-07-28" {
		t.Errorf("daily span = %s..%s, want 2026-07-22..2026-07-28", report.Daily[0].Day, report.Daily[6].Day)
	}
	for i, p := range report.Daily {
		if wantPartial := i == 6; p.Partial != wantPartial {
			t.Errorf("daily[%d].partial = %v, want %v", i, p.Partial, wantPartial)
		}
	}
	// A day with no rows is an explicit zero, never "unavailable".
	if got := mustValue(t, report.Daily[0].Sessions); got != 0 {
		t.Errorf("empty day sessions = %d, want 0", got)
	}
	if got := mustValue(t, report.Daily[5].Sessions); got != 40 {
		t.Errorf("2026-07-27 sessions = %d, want 40", got)
	}
	if got := mustValue(t, report.Daily[5].Devices); got != 20 {
		t.Errorf("2026-07-27 devices = %d, want 20", got)
	}

	// Breakdowns follow the fixed contract order and expose only the persisted
	// dimensions.
	if len(report.Breakdowns) != len(reportDimensions) {
		t.Fatalf("breakdowns = %d, want %d", len(report.Breakdowns), len(reportDimensions))
	}
	for i, b := range report.Breakdowns {
		if b.Dimension != reportDimensions[i] {
			t.Errorf("breakdown[%d] = %q, want %q", i, b.Dimension, reportDimensions[i])
		}
		if !b.Other.Available() || *b.Other.Value != 0 {
			t.Errorf("breakdown %q other = %+v, want available 0", b.Dimension, b.Other)
		}
	}
	commands := breakdownOf(t, report, DimCommand)
	if len(commands.Buckets) != 3 || commands.Buckets[0].Key != "build" || commands.Buckets[0].Count != 60 {
		t.Errorf("command buckets = %+v, want build first with 60", commands.Buckets)
	}

	// Freshness is the latest day with published data.
	if report.Freshness.State != StateAvailable || report.Freshness.LatestDay != "2026-07-28" {
		t.Errorf("freshness = %+v, want available 2026-07-28", report.Freshness)
	}
	if report.Freshness.GeneratedAt != "2026-07-28T15:04:05Z" {
		t.Errorf("generatedAt = %q, want 2026-07-28T15:04:05Z", report.Freshness.GeneratedAt)
	}
}

// TestIngestHealthIsUnavailableNeverZero pins requirement 3: a field with no
// authoritative persisted counter reports unavailable, never an inferred zero.
func TestIngestHealthIsUnavailableNeverZero(t *testing.T) {
	report := buildReport(window(t, Window7d), healthyRaw(), fixedNow)
	for name, c := range map[string]Count{
		"acceptedRecords": report.Summary.Ingest.AcceptedRecords,
		"rejectedRecords": report.Summary.Ingest.RejectedRecords,
	} {
		if c.State != StateUnavailable {
			t.Errorf("ingest.%s state = %q, want %q", name, c.State, StateUnavailable)
		}
		if c.Value != nil {
			t.Errorf("ingest.%s carries a value %d; an unavailable field must never look like zero", name, *c.Value)
		}
		if c.Reason != ReasonNotPersisted {
			t.Errorf("ingest.%s reason = %q, want %q", name, c.Reason, ReasonNotPersisted)
		}
	}

	// The same report distinguishes a real zero from unavailable.
	zeroDay := report.Daily[0]
	if !zeroDay.Sessions.Available() || *zeroDay.Sessions.Value != 0 {
		t.Fatalf("a day with no rows must be an available zero, got %+v", zeroDay.Sessions)
	}
}

// TestSuppressionAcrossAllThreeSurfaces pins requirement 4: a cohort below the
// threshold is withheld from the summary, the daily series, AND the breakdowns.
func TestSuppressionAcrossAllThreeSurfaces(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "small-groups-are-withheld", "a-group-below-the-contributor-threshold-is-withheld")
	data := raw{
		devices:       3, // below threshold
		dailyDevices:  map[string]int64{"2026-07-27": 3, "2026-07-28": 2},
		dailySessions: map[string]int64{"2026-07-27": 4, "2026-07-28": 2},
		counters: map[string]map[string]int64{
			DimCommand:     {"build": 4, "deploy": 2},
			DimOutcome:     {"success": 4, "error:api": 2},
			DimCLIVersion:  {"1.2.3": 6},
			DimOS:          {"darwin": 6},
			DimArch:        {"arm64": 6},
			DimInteractive: {"true": 6},
		},
	}
	report := buildReport(window(t, Window7d), data, fixedNow)

	if report.Summary.Devices.State != StateSuppressed {
		t.Errorf("summary.devices = %+v, want suppressed", report.Summary.Devices)
	}
	// Event mass cannot make a contributor-small cohort safe: the same person
	// may be responsible for every repeat, so coupled totals fail closed too.
	if report.Summary.Sessions.State != StateSuppressed {
		t.Errorf("summary.sessions = %+v, want suppressed", report.Summary.Sessions)
	}
	if report.Summary.Commands.State != StateSuppressed {
		t.Errorf("summary.commands = %+v, want suppressed", report.Summary.Commands)
	}
	for _, b := range report.Breakdowns {
		if len(b.Buckets) != 0 && b.Dimension != DimCLIVersion && b.Dimension != DimOS && b.Dimension != DimArch && b.Dimension != DimInteractive {
			t.Errorf("breakdown %q published %+v, want every small cohort withheld", b.Dimension, b.Buckets)
		}
	}
	for _, p := range report.Daily {
		if p.Day != "2026-07-27" && p.Day != "2026-07-28" {
			continue
		}
		if p.Devices.State != StateSuppressed || p.Sessions.State != StateSuppressed {
			t.Errorf("daily %s = devices %+v sessions %+v, want both suppressed", p.Day, p.Devices, p.Sessions)
		}
	}
	for _, b := range report.Breakdowns {
		for _, bucket := range b.Buckets {
			if bucket.Count < SuppressionThreshold {
				t.Errorf("breakdown %q published bucket %q with %d < %d", b.Dimension, bucket.Key, bucket.Count, SuppressionThreshold)
			}
		}
	}
	// Every published number in the report is either zero or at least k.
	assertNoSmallCounts(t, report)
}

func TestRepeatedEventsFromOneDeviceRemainSuppressed(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "small-groups-are-withheld", "repeating-an-event-does-not-raise-the-contributor-count")
	data := healthyRaw()
	data.counters[DimCommand] = map[string]int64{"build": 5}
	data.counterContributors = map[string]map[string]int64{
		DimCommand: {"build": 1},
	}
	data.dimensionContributors = map[string]int64{DimCommand: 1, DimOutcome: 42}
	report := buildReport(window(t, Window7d), data, fixedNow)

	commands := breakdownOf(t, report, DimCommand)
	if len(commands.Buckets) != 0 {
		t.Fatalf("one device repeating five times published a cohort: %+v", commands.Buckets)
	}
	if report.Summary.Commands.State != StateSuppressed {
		t.Fatalf("command total = %+v, want suppressed for one contributor", report.Summary.Commands)
	}

	data.counterContributors[DimCommand]["build"] = 5
	data.dimensionContributors[DimCommand] = 5
	report = buildReport(window(t, Window7d), data, fixedNow)
	commands = breakdownOf(t, report, DimCommand)
	if len(commands.Buckets) != 1 || commands.Buckets[0].Count != 5 {
		t.Fatalf("five distinct contributors should publish exact event count: %+v", commands.Buckets)
	}
}

// TestDifferencingCannotReconstructASuppressedCohort is the adversary test the
// privacy boundary exists for. The outcome dimension hides exactly one cohort
// (error:api = 3). If the summary total were published, the caller would derive
// 43 - 40 = 3 and learn the suppressed cohort exactly. The contract must
// withhold the total and every residual of the same quantity instead.
func TestDifferencingCannotReconstructASuppressedCohort(t *testing.T) {
	data := raw{
		devices:       25,
		dailyDevices:  map[string]int64{"2026-07-27": 12, "2026-07-28": 13},
		dailySessions: map[string]int64{"2026-07-27": 20, "2026-07-28": 23},
		counters: map[string]map[string]int64{
			DimCommand:     {"build": 40, "test": 20},
			DimOutcome:     {"success": 40, "error:api": 3},
			DimCLIVersion:  {"1.2.3": 43},
			DimOS:          {"darwin": 43},
			DimArch:        {"arm64": 43},
			DimInteractive: {"true": 43},
		},
	}
	report := buildReport(window(t, Window7d), data, fixedNow)

	if report.Summary.Sessions.State != StateSuppressed {
		t.Fatalf("summary.sessions = %+v; publishing the total leaks 43-40=3", report.Summary.Sessions)
	}
	// Every partition must withhold its residual too: cli_version/os/arch each
	// have a single visible cohort of 43, and the command mix is a usable proxy
	// for the same total, so any published residual re-opens the subtraction.
	for _, dim := range reportDimensions {
		b := breakdownOf(t, report, dim)
		if b.Other.State != StateSuppressed {
			t.Errorf("breakdown %q other = %+v; publishing it discloses the session total", dim, b.Other)
		}
	}
	if report.Summary.Commands.State != StateSuppressed {
		t.Errorf("summary.commands = %+v; a one-command session makes it a proxy for the session total", report.Summary.Commands)
	}
	// The distribution itself survives: cohorts at or above the threshold are
	// still published exactly.
	if b := breakdownOf(t, report, DimOutcome); len(b.Buckets) != 1 || b.Buckets[0].Count != 40 {
		t.Errorf("outcome buckets = %+v, want success=40 still visible", b.Buckets)
	}

	assertNoDerivableSuppressedCohort(t, report, data)
	assertNoSmallCounts(t, report)
}

// TestDailySeriesResidualCannotBeDifferenced covers the second differencing
// route: the daily series is a partition of the same session total, and the
// days it withholds are identifiable by position, so a published total would
// name them.
func TestDailySeriesResidualCannotBeDifferenced(t *testing.T) {
	data := raw{
		devices:       30,
		dailyDevices:  map[string]int64{"2026-07-26": 15, "2026-07-27": 15, "2026-07-28": 6},
		dailySessions: map[string]int64{"2026-07-26": 50, "2026-07-27": 50, "2026-07-28": 3},
		counters: map[string]map[string]int64{
			DimOutcome:     {"success": 103},
			DimCLIVersion:  {"1.2.3": 103},
			DimOS:          {"darwin": 103},
			DimArch:        {"arm64": 103},
			DimInteractive: {"true": 103},
			DimCommand:     {"build": 103},
		},
	}
	report := buildReport(window(t, Window7d), data, fixedNow)

	if report.Summary.Sessions.State != StateSuppressed {
		t.Fatalf("summary.sessions = %+v; 103 minus the two visible days reveals 2026-07-28 = 3", report.Summary.Sessions)
	}
	for _, dim := range reportDimensions {
		if b := breakdownOf(t, report, dim); b.Other.State != StateSuppressed {
			t.Errorf("breakdown %q other = %+v; it would re-expose the session total", dim, b.Other)
		}
	}
	// The visible days stay exact — they are their own publishable cohorts.
	for _, p := range report.Daily {
		switch p.Day {
		case "2026-07-26", "2026-07-27":
			if got := mustValue(t, p.Sessions); got != 50 {
				t.Errorf("daily %s = %d, want 50", p.Day, got)
			}
		case "2026-07-28":
			if p.Sessions.State != StateSuppressed {
				t.Errorf("daily %s = %+v, want suppressed", p.Day, p.Sessions)
			}
		}
	}
	assertNoDerivableSuppressedCohort(t, report, data)
}

// TestResidualIsPublishedOnlyWhenItSpansEnoughCohorts proves the positive case:
// when the withheld mass reaches the threshold it is publishable, because
// primary suppression only ever withholds cohorts below the threshold, so a
// residual at or above it necessarily spans two or more of them.
func TestResidualIsPublishedOnlyWhenItSpansEnoughCohorts(t *testing.T) {
	data := raw{
		devices:       25,
		dailyDevices:  map[string]int64{"2026-07-27": 12, "2026-07-28": 13},
		dailySessions: map[string]int64{"2026-07-27": 20, "2026-07-28": 27},
		counters: map[string]map[string]int64{
			DimCommand:     {"build": 47},
			DimOutcome:     {"success": 40, "error:api": 3, "error:usage": 4},
			DimCLIVersion:  {"1.2.3": 47},
			DimOS:          {"darwin": 47},
			DimArch:        {"arm64": 47},
			DimInteractive: {"true": 47},
		},
	}
	report := buildReport(window(t, Window7d), data, fixedNow)

	if report.Summary.Sessions.State != StateSuppressed {
		t.Fatalf("summary.sessions = %+v, want suppressed", report.Summary.Sessions)
	}
	outcome := breakdownOf(t, report, DimOutcome)
	if len(outcome.Buckets) != 1 || outcome.Buckets[0].Key != "success" {
		t.Fatalf("outcome buckets = %+v, want only success", outcome.Buckets)
	}
	if outcome.Other.State != StateSuppressed {
		t.Fatalf("outcome other = %+v, want suppressed", outcome.Other)
	}
	assertNoDerivableSuppressedCohort(t, report, data)
	assertNoSmallCounts(t, report)
}

// TestBreakdownCardinalityIsBounded proves the response size is a function of
// the contract, not of the data: extra cohorts fold into the residual.
func TestBreakdownCardinalityIsBounded(t *testing.T) {
	commands := map[string]int64{}
	for i := 0; i < maxBucketsPerDimension+20; i++ {
		commands["cmd"+strconv.Itoa(i)] = int64(10 + i)
	}
	data := healthyRaw()
	data.counters[DimCommand] = commands
	report := buildReport(window(t, Window7d), data, fixedNow)

	b := breakdownOf(t, report, DimCommand)
	if len(b.Buckets) != maxBucketsPerDimension {
		t.Fatalf("buckets = %d, want the %d-bucket bound", len(b.Buckets), maxBucketsPerDimension)
	}
	if !b.Other.Available() || *b.Other.Value < SuppressionThreshold {
		t.Fatalf("other = %+v, want the folded tail", b.Other)
	}
	assertNoSmallCounts(t, report)
}

// TestUnexpectedKeysNeverCrossTheBoundary pins the free-text defense: a key
// outside the closed vocabulary shape is folded into the residual instead of
// being echoed, even if its count is large.
func TestUnexpectedKeysNeverCrossTheBoundary(t *testing.T) {
	hostile := []string{
		"user@example.com",
		"192.0.2.7",
		"2001:db8::1",
		"/Users/someone/src/private-project",
		"free text with spaces",
		strings.Repeat("a", maxKeyLength+1),
		"<script>alert(1)</script>",
	}
	counters := map[string]int64{"build": 100}
	for _, k := range hostile {
		counters[k] = 100
	}
	data := healthyRaw()
	data.counters[DimCommand] = counters
	report := buildReport(window(t, Window7d), data, fixedNow)

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range hostile {
		if strings.Contains(string(body), k) {
			t.Errorf("key %q crossed the API boundary: %s", k, body)
		}
	}
	if b := breakdownOf(t, report, DimCommand); len(b.Buckets) != 1 || b.Buckets[0].Key != "build" {
		t.Errorf("command buckets = %+v, want only build", b.Buckets)
	}
}

// TestFreshnessNeverDisclosesASuppressedDay proves the freshness field is
// derived only from days whose own data is published.
func TestFreshnessNeverDisclosesASuppressedDay(t *testing.T) {
	t.Run("latest day is a published day", func(t *testing.T) {
		data := raw{
			devices:       20,
			dailyDevices:  map[string]int64{"2026-07-26": 20, "2026-07-28": 2},
			dailySessions: map[string]int64{"2026-07-26": 40, "2026-07-28": 2},
			counters: map[string]map[string]int64{
				DimOutcome: {"success": 42},
			},
		}
		report := buildReport(window(t, Window7d), data, fixedNow)
		if report.Freshness.State != StateAvailable || report.Freshness.LatestDay != "2026-07-26" {
			t.Fatalf("freshness = %+v, want available 2026-07-26 (2026-07-28 is suppressed)", report.Freshness)
		}
	})

	t.Run("nothing published means unavailable, not zero", func(t *testing.T) {
		data := raw{
			devices:       2,
			dailyDevices:  map[string]int64{"2026-07-28": 2},
			dailySessions: map[string]int64{"2026-07-28": 2},
			counters:      map[string]map[string]int64{DimOutcome: {"success": 2}},
		}
		report := buildReport(window(t, Window7d), data, fixedNow)
		if report.Freshness.State != StateUnavailable || report.Freshness.Reason != ReasonNoPublishedData {
			t.Fatalf("freshness = %+v, want unavailable/%s", report.Freshness, ReasonNoPublishedData)
		}
		if report.Freshness.LatestDay != "" {
			t.Fatalf("latestDay = %q, want empty", report.Freshness.LatestDay)
		}
	})
}

// TestEmptyProjectionIsZeroNotUnavailable pins that "no data at all" still
// answers with a well-formed, all-zero report.
func TestEmptyProjectionIsZeroNotUnavailable(t *testing.T) {
	report := buildReport(window(t, Window1d), raw{
		dailyDevices:  map[string]int64{},
		dailySessions: map[string]int64{},
		counters:      map[string]map[string]int64{},
	}, fixedNow)

	if got := mustValue(t, report.Summary.Devices); got != 0 {
		t.Errorf("devices = %d, want 0", got)
	}
	if got := mustValue(t, report.Summary.Sessions); got != 0 {
		t.Errorf("sessions = %d, want 0", got)
	}
	if len(report.Daily) != 1 || report.Daily[0].Day != "2026-07-28" || !report.Daily[0].Partial {
		t.Errorf("daily = %+v, want a single partial bucket for 2026-07-28", report.Daily)
	}
	for _, b := range report.Breakdowns {
		if len(b.Buckets) != 0 {
			t.Errorf("breakdown %q = %+v, want no buckets", b.Dimension, b.Buckets)
		}
		if !b.Other.Available() || *b.Other.Value != 0 {
			t.Errorf("breakdown %q other = %+v, want available 0", b.Dimension, b.Other)
		}
	}
}

func TestDimensionOverflowDoesNotHideHealthyCohorts(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "exhausted-dimension-is-reported-unavailable", "unaffected-dimensions-stay-available")
	data := healthyRaw()
	data.unavailable = map[string]bool{DimCLIVersion: true}
	report := buildReport(window(t, Window7d), data, fixedNow)

	if !report.Summary.Devices.Available() ||
		!report.Summary.Sessions.Available() ||
		!report.Summary.Commands.Available() {
		t.Fatalf("healthy fixed cohorts became unavailable: %+v", report.Summary)
	}
	version := breakdownOf(t, report, DimCLIVersion)
	if version.State != StateUnavailable || version.Reason != ReasonProjectionOverflow ||
		version.Other.Reason != ReasonProjectionOverflow || len(version.Buckets) != 0 {
		t.Fatalf("cli-version overflow breakdown = %+v", version)
	}
	outcome := breakdownOf(t, report, DimOutcome)
	if outcome.State != StateAvailable || len(outcome.Buckets) == 0 {
		t.Fatalf("healthy outcome breakdown was hidden: %+v", outcome)
	}
}

func TestDeviceOverflowOnlyHidesDeviceCounts(t *testing.T) {
	data := healthyRaw()
	data.unavailable = map[string]bool{DimDevices: true}
	report := buildReport(window(t, Window7d), data, fixedNow)
	if report.Summary.Devices.State != StateUnavailable ||
		report.Summary.Devices.Reason != ReasonProjectionOverflow {
		t.Fatalf("summary devices = %+v", report.Summary.Devices)
	}
	if !report.Summary.Sessions.Available() || !report.Summary.Commands.Available() {
		t.Fatalf("non-device summaries became unavailable: %+v", report.Summary)
	}
	for _, point := range report.Daily {
		if point.Devices.State != StateUnavailable || !point.Sessions.Available() {
			t.Fatalf("daily point did not scope device overflow: %+v", point)
		}
	}
}

func TestLegacyGlobalOverflowFailsEveryProjectionClosed(t *testing.T) {
	data := healthyRaw()
	data.unavailable = map[string]bool{"*": true}
	report := buildReport(window(t, Window7d), data, fixedNow)
	for name, count := range map[string]Count{
		"devices":  report.Summary.Devices,
		"sessions": report.Summary.Sessions,
		"commands": report.Summary.Commands,
	} {
		if count.State != StateUnavailable || count.Reason != ReasonProjectionOverflow {
			t.Errorf("summary %s = %+v", name, count)
		}
	}
	for _, breakdown := range report.Breakdowns {
		if breakdown.State != StateUnavailable || breakdown.Reason != ReasonProjectionOverflow {
			t.Errorf("breakdown %s = %+v", breakdown.Dimension, breakdown)
		}
	}
}

// TestReportDimensionsTrackTheWriteSide keeps the read vocabulary pinned to the
// Dim* constants the accumulator writes, so the two cannot drift.
func TestReportDimensionsTrackTheWriteSide(t *testing.T) {
	// The write side tallies exactly these dimensions (see Accumulator.addRecord).
	writeSide := []string{
		DimCommand, DimOutcome, DimCLIVersion, DimOS, DimArch, DimInteractive,
		DimProjects, DimJobs, DimDuration, DimFlag,
	}
	if len(reportDimensions) != len(writeSide) {
		t.Fatalf("reportDimensions = %v, want every persisted dimension %v", reportDimensions, writeSide)
	}
	for _, dim := range writeSide {
		if !contains(reportDimensions, dim) {
			t.Errorf("dimension %q is persisted but never published", dim)
		}
	}
	for _, dim := range Dimensions() {
		if !contains(writeSide, dim) {
			t.Errorf("dimension %q is published but never persisted", dim)
		}
	}
	// Dimensions() must not hand out the package's own slice.
	if published := Dimensions(); len(published) > 0 {
		published[0] = "mutated"
		if reportDimensions[0] == "mutated" {
			t.Error("Dimensions() exposes the contract vocabulary for mutation")
		}
	}
}

// TestReportCarriesNoIdentifierField walks the serialized contract and fails on
// any field that could carry a device id, an IP, or a raw event.
func TestReportCarriesNoIdentifierField(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "caller-address-is-not-retained", "no-caller-address-reaches-an-aggregate")
	body, err := json.Marshal(buildReport(window(t, Window30d), healthyRaw(), fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"device_id", "deviceid", // no identifier, ever
		"devices\":[", // devices is a count, never a list
		"\"ip\"", "ipaddress", "remoteaddr",
		"cursor", "nextpage", // no pagination over identities
		"datasource", "dsn", "workspace", // no steering target
		// The "projects" dimension and flag carry only a count range or a
		// presence tally; no field may name or identify a project.
		"\"project\"", "project_id", "projectid", "project_name", "projectname",
		"rawevents", "\"records\":[",
	}
	lower := strings.ToLower(string(body))
	for _, f := range forbidden {
		if strings.Contains(lower, f) {
			t.Errorf("report contains forbidden fragment %q: %s", f, body)
		}
	}
}

// TestSuppressPartitionInvariants unit-tests the rule every surface is built
// on, including the property the residual publication depends on: a publishable
// residual always spans at least two withheld cohorts.
func TestSuppressPartitionInvariants(t *testing.T) {
	tests := []struct {
		name         string
		cohorts      map[string]int64
		wantVisible  []string
		wantResidual int64
		wantTotal    int64
		wantSafe     bool
	}{
		{name: "empty", cohorts: map[string]int64{}, wantSafe: true},
		{
			name:        "everything publishable",
			cohorts:     map[string]int64{"a": 10, "b": 5},
			wantVisible: []string{"a", "b"}, wantTotal: 15, wantSafe: true,
		},
		{
			name:        "threshold is inclusive",
			cohorts:     map[string]int64{"a": 5, "b": 4},
			wantVisible: []string{"a"}, wantResidual: 4, wantTotal: 9, wantSafe: false,
		},
		{
			name:        "one small cohort withholds the residual",
			cohorts:     map[string]int64{"a": 40, "b": 3},
			wantVisible: []string{"a"}, wantResidual: 3, wantTotal: 43, wantSafe: false,
		},
		{
			name:        "event mass across small cohorts remains unsafe",
			cohorts:     map[string]int64{"a": 40, "b": 3, "c": 4},
			wantVisible: []string{"a"}, wantResidual: 7, wantTotal: 47, wantSafe: false,
		},
		{
			name:        "an empty tally is not a cohort",
			cohorts:     map[string]int64{"a": 40, "b": 0},
			wantVisible: []string{"a"}, wantResidual: 0, wantTotal: 40, wantSafe: true,
		},
		{
			name:        "an unpublishable key is withheld whatever its size",
			cohorts:     map[string]int64{"a": 40, "leaked path/here": 1000},
			wantVisible: []string{"a"}, wantResidual: 1000, wantTotal: 1040, wantSafe: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := suppress(cohortsOf(tc.cohorts, nil), maxBucketsPerDimension)

			var visible []string
			for _, b := range p.visible {
				visible = append(visible, b.Key)
				if b.Count < SuppressionThreshold {
					t.Errorf("published bucket %q with %d, below the threshold", b.Key, b.Count)
				}
			}
			if strings.Join(visible, ",") != strings.Join(tc.wantVisible, ",") {
				t.Errorf("visible = %v, want %v", visible, tc.wantVisible)
			}
			if p.residual != tc.wantResidual {
				t.Errorf("residual = %d, want %d", p.residual, tc.wantResidual)
			}
			if p.total != tc.wantTotal {
				t.Errorf("total = %d, want %d", p.total, tc.wantTotal)
			}
			if p.safe != tc.wantSafe {
				t.Errorf("safe = %v, want %v", p.safe, tc.wantSafe)
			}
			// The properties residual publication rests on: a published residual
			// is never itself below the threshold, and when every withheld
			// cohort is a small one it necessarily spans at least two of them.
			if p.safe && p.residual > 0 && p.residual < SuppressionThreshold {
				t.Errorf("publishable residual %d is below the threshold", p.residual)
			}
			visibleSet := map[string]bool{}
			for _, b := range p.visible {
				visibleSet[b.Key] = true
			}
			smallOnly := true
			for key, count := range tc.cohorts {
				if !visibleSet[key] && count >= SuppressionThreshold {
					smallOnly = false
				}
			}
			if smallOnly && p.safe && p.residual > 0 && p.hidden < 2 {
				t.Errorf("a publishable residual of %d spans only %d small cohort(s)", p.residual, p.hidden)
			}
			if sum := sumBuckets(p.visible) + p.residual; sum != p.total {
				t.Errorf("visible+residual = %d, want the total %d", sum, p.total)
			}
		})
	}
}

func sumBuckets(buckets []Bucket) int64 {
	var sum int64
	for _, b := range buckets {
		sum += b.Count
	}
	return sum
}

// --- assertions shared by the privacy tests ---

// assertNoSmallCounts checks the global invariant: every number the report
// publishes is either zero or at least the suppression threshold.
func assertNoSmallCounts(t *testing.T, r Report) {
	t.Helper()
	check := func(name string, c Count) {
		if !c.Available() {
			return
		}
		if v := *c.Value; v != 0 && v < SuppressionThreshold {
			t.Errorf("%s published %d, below the threshold %d", name, v, SuppressionThreshold)
		}
	}
	check("summary.devices", r.Summary.Devices)
	check("summary.sessions", r.Summary.Sessions)
	check("summary.commands", r.Summary.Commands)
	check("summary.ingest.accepted", r.Summary.Ingest.AcceptedRecords)
	check("summary.ingest.rejected", r.Summary.Ingest.RejectedRecords)
	for _, p := range r.Daily {
		check("daily."+p.Day+".devices", p.Devices)
		check("daily."+p.Day+".sessions", p.Sessions)
	}
	for _, b := range r.Breakdowns {
		check("breakdown."+b.Dimension+".other", b.Other)
		for _, bucket := range b.Buckets {
			if bucket.Count < SuppressionThreshold {
				t.Errorf("breakdown.%s bucket %q published %d, below the threshold", b.Dimension, bucket.Key, bucket.Count)
			}
		}
	}
}

// assertNoDerivableSuppressedCohort replays the adversary's arithmetic: for
// every partition of the session total, subtract the published buckets from the
// published total and check the answer is never a withheld cohort's value.
func assertNoDerivableSuppressedCohort(t *testing.T, r Report, data raw) {
	t.Helper()

	partitions := map[string]map[string]int64{"daily": data.dailySessions}
	for _, dim := range reportDimensions {
		partitions[dim] = data.counters[dim]
	}

	// Collect the exact values of every cohort the report withheld.
	withheld := map[string][]int64{}
	for name, cohorts := range partitions {
		published := publishedKeys(r, name)
		for key, count := range cohorts {
			if !published[key] {
				withheld[name] = append(withheld[name], count)
			}
		}
	}

	derivable := []int64{}
	for _, total := range []Count{r.Summary.Sessions, r.Summary.Commands} {
		if !total.Available() {
			continue
		}
		for name := range partitions {
			derivable = append(derivable, *total.Value-publishedSum(r, name))
		}
	}
	for _, b := range r.Breakdowns {
		if b.Other.Available() {
			derivable = append(derivable, *b.Other.Value)
		}
	}

	for _, d := range derivable {
		if d == 0 || d >= SuppressionThreshold {
			continue // an aggregate at or above the threshold is publishable
		}
		for name, values := range withheld {
			for _, v := range values {
				if v == d {
					t.Fatalf("a caller can derive %d by differencing, reconstructing a withheld %s cohort", d, name)
				}
			}
		}
		t.Fatalf("a caller can derive the below-threshold value %d by differencing", d)
	}
}

// publishedKeys lists the cohort keys a partition published.
func publishedKeys(r Report, partition string) map[string]bool {
	out := map[string]bool{}
	if partition == "daily" {
		for _, p := range r.Daily {
			if p.Sessions.Available() {
				out[p.Day] = true
			}
		}
		return out
	}
	for _, b := range r.Breakdowns {
		if b.Dimension != partition {
			continue
		}
		for _, bucket := range b.Buckets {
			out[bucket.Key] = true
		}
	}
	return out
}

// publishedSum totals the values a partition published.
func publishedSum(r Report, partition string) int64 {
	var sum int64
	if partition == "daily" {
		for _, p := range r.Daily {
			if p.Sessions.Available() {
				sum += *p.Sessions.Value
			}
		}
		return sum
	}
	for _, b := range r.Breakdowns {
		if b.Dimension != partition {
			continue
		}
		for _, bucket := range b.Buckets {
			sum += bucket.Count
		}
	}
	return sum
}

func breakdownOf(t *testing.T, r Report, dimension string) Breakdown {
	t.Helper()
	for _, b := range r.Breakdowns {
		if b.Dimension == dimension {
			return b
		}
	}
	t.Fatalf("no breakdown for dimension %q", dimension)
	return Breakdown{}
}

func mustValue(t *testing.T, c Count) int64 {
	t.Helper()
	if !c.Available() {
		t.Fatalf("count is not available: %+v", c)
	}
	return *c.Value
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
