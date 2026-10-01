package cliagg

import (
	"go.putnami.dev/protocol/features/spectest"

	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"
)

// runStart is a session:start carrying the run-shape fields the CLI sends.
func runStart(ts, device string, projects, jobs int64, flags ...string) telemetry.LogRecord {
	rec := sessionStart(ts, device, "build", true)
	rec.Attributes = append(rec.Attributes,
		telemetry.Attr(cliusage.AttrProjects, telemetry.IntVal(projects)),
		telemetry.Attr(cliusage.AttrJobs, telemetry.IntVal(jobs)),
	)
	set := make(map[string]bool, len(flags))
	for _, flag := range flags {
		set[flag] = true
	}
	for _, flag := range flagKeys {
		rec.Attributes = append(rec.Attributes, telemetry.Attr(flag.attr, telemetry.BoolVal(set[flag.attr])))
	}
	return rec
}

// runEnd is a successful session:end carrying a duration in milliseconds.
func runEnd(ts, device string, durationMS int64) telemetry.LogRecord {
	rec := sessionEnd(ts, device, "1.2.3", "darwin", "arm64", true, true, "")
	rec.Attributes = append(rec.Attributes, telemetry.Attr(cliusage.AttrDuration, telemetry.IntVal(durationMS)))
	return rec
}

func TestRunShapeIsFoldedIntoRanges(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "sent-fields-are-aggregated", "run-shape-fields-are-folded-into-fixed-ranges")

	acc := testAccumulator()
	a := nanosAt(dayA)
	acc.Add([]telemetry.ResourceLogs{cliResource(
		runStart(a, "d1", 12, 949, cliusage.AttrFlagImpacted, cliusage.AttrFlagCoverage),
		runEnd(a, "d1", 45_000),
		runStart(a, "d2", 1, 3),
		runEnd(a, "d2", 400),
	)})
	snap := acc.Drain()

	got := make(map[[3]string]int64)
	for key, count := range counterMap(snap.Counters) {
		switch key[1] {
		case DimProjects, DimJobs, DimDuration, DimFlag:
			got[key] = count
		}
	}
	want := map[[3]string]int64{
		{"2026-07-25", DimProjects, "6-20"}:   1,
		{"2026-07-25", DimProjects, "1"}:      1,
		{"2026-07-25", DimJobs, "501-2000"}:   1,
		{"2026-07-25", DimJobs, "2-5"}:        1,
		{"2026-07-25", DimFlag, "impacted"}:   1,
		{"2026-07-25", DimFlag, "coverage"}:   1,
		{"2026-07-25", DimDuration, "30s-2m"}: 1,
		{"2026-07-25", DimDuration, "0-1s"}:   1,
	}
	if len(got) != len(want) {
		t.Fatalf("run-shape counters = %v, want %v", got, want)
	}
	for key, count := range want {
		if got[key] != count {
			t.Fatalf("counter %v = %d, want %d (all: %v)", key, got[key], count, got)
		}
	}

	// Every run-shape tally carries its contributor, so the report can apply
	// the same distinct-contributor rule as the original dimensions.
	contributors := 0
	for _, c := range snap.Contributors {
		switch c.Dimension {
		case DimProjects, DimJobs, DimDuration, DimFlag:
			contributors++
		}
	}
	if contributors != len(want) {
		t.Fatalf("run-shape contributors = %d, want %d", contributors, len(want))
	}
}

func TestRangeKeyBoundaries(t *testing.T) {
	cases := []struct {
		ranges []sizeRange
		n      int64
		want   string
	}{
		{projectRanges, -3, "0"},
		{projectRanges, 0, "0"},
		{projectRanges, 1, "1"},
		{projectRanges, 5, "2-5"},
		{projectRanges, 6, "6-20"},
		{projectRanges, 100, "51-100"},
		{projectRanges, 501, "501+"},
		{jobRanges, 2000, "501-2000"},
		{jobRanges, 2001, "2001+"},
		{durationRanges, 999, "0-1s"},
		{durationRanges, 1_000, "1-5s"},
		{durationRanges, 1_799_999, "10-30m"},
		{durationRanges, 1_800_000, "30m+"},
	}
	for _, tc := range cases {
		if got := rangeKey(tc.ranges, tc.n); got != tc.want {
			t.Errorf("rangeKey(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
	for _, ranges := range [][]sizeRange{projectRanges, jobRanges, durationRanges} {
		if len(ranges) > counterDimensionCaps[DimProjects] {
			t.Fatalf("%d ranges exceed the dimension cap", len(ranges))
		}
		for i, r := range ranges {
			if !publishableKey(r.key) {
				t.Fatalf("range key %q is not publishable", r.key)
			}
			if i > 0 && r.min <= ranges[i-1].min {
				t.Fatalf("ranges are not ordered by lower bound at %q", r.key)
			}
		}
	}
	if len(flagKeys) > counterDimensionCaps[DimFlag] {
		t.Fatalf("%d flags exceed the flag dimension cap", len(flagKeys))
	}
}

func TestRunShapeIgnoresMalformedValues(t *testing.T) {
	acc := testAccumulator()
	rec := sessionStart(nanosAt(dayA), "d1", "build", true)
	bad := "twelve"
	rec.Attributes = append(rec.Attributes,
		telemetry.Attr(cliusage.AttrProjects, telemetry.AnyValue{IntValue: &bad}),
		telemetry.Attr(cliusage.AttrJobs, telemetry.StringVal("3")),
	)
	acc.Add([]telemetry.ResourceLogs{cliResource(rec)})
	for _, c := range acc.Drain().Counters {
		if c.Dimension == DimProjects || c.Dimension == DimJobs {
			t.Fatalf("malformed run-shape value was counted: %+v", c)
		}
	}
}

// The database enforces the same per-dimension caps as the accumulator. The
// newest migration that defines each cap function is the one in force.
func TestSQLCapsMirrorAccumulatorCaps(t *testing.T) {
	for fn, want := range map[string]map[string]int{
		"cli_counter_dimension_cap":     counterDimensionCaps,
		"cli_contributor_dimension_cap": contributorDimensionCaps,
	} {
		got := latestSQLCaps(t, fn)
		if len(got) != len(want) {
			t.Fatalf("%s defines %v, accumulator defines %v", fn, got, want)
		}
		for dim, capacity := range want {
			if got[dim] != capacity {
				t.Fatalf("%s(%q) = %d, accumulator cap %d", fn, dim, got[dim], capacity)
			}
		}
	}
}

func latestSQLCaps(t *testing.T, fn string) map[string]int {
	t.Helper()
	names, err := fs.Glob(migrationsFS, "migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	whenThen := regexp.MustCompile(`WHEN '([a-z_]+)' THEN ([0-9]+)`)
	for i := len(names) - 1; i >= 0; i-- {
		data, err := migrationsFS.ReadFile(names[i])
		if err != nil {
			t.Fatal(err)
		}
		sql := string(data)
		start := strings.Index(sql, "FUNCTION "+fn+"(")
		if start < 0 {
			continue
		}
		body := sql[start:]
		if end := strings.Index(body, "END\n$$"); end >= 0 {
			body = body[:end]
		}
		caps := make(map[string]int)
		for _, m := range whenThen.FindAllStringSubmatch(body, -1) {
			n, _ := strconv.Atoi(m[2])
			caps[m[1]] = n
		}
		return caps
	}
	t.Fatalf("no migration defines %s", fn)
	return nil
}
