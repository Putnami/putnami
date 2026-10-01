package cliagg

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"
)

// Fixed calendar days used across the fixtures.
var (
	dayA = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	dayB = time.Date(2026, 7, 26, 9, 30, 0, 0, time.UTC)
)

func nanosAt(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }
func testAccumulator() *Accumulator {
	return newAccumulator(func() time.Time {
		return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	})
}

// cliResource wraps records in a service.name=putnami-cli resource.
func cliResource(records ...telemetry.LogRecord) telemetry.ResourceLogs {
	return telemetry.ResourceLogs{
		Resource: telemetry.Resource{Attributes: []telemetry.KeyValue{
			telemetry.Attr(telemetry.AttrServiceName, telemetry.StringVal(cliusage.ServiceName)),
		}},
		ScopeLogs: []telemetry.ScopeLogs{{LogRecords: records}},
	}
}

func envelope(device, cliVersion, os, arch string) []telemetry.KeyValue {
	return []telemetry.KeyValue{
		telemetry.Attr(cliusage.AttrDeviceID, telemetry.StringVal(device)),
		telemetry.Attr(cliusage.AttrCLIVersion, telemetry.StringVal(cliVersion)),
		telemetry.Attr(cliusage.AttrOS, telemetry.StringVal(os)),
		telemetry.Attr(cliusage.AttrArch, telemetry.StringVal(arch)),
	}
}

func sessionStart(ts, device, commands string, interactive bool) telemetry.LogRecord {
	attrs := envelope(device, "x", "x", "x")
	attrs = append(attrs,
		telemetry.Attr(cliusage.AttrEventName, telemetry.StringVal(cliusage.EventSessionStart)),
		telemetry.Attr(cliusage.AttrCommands, telemetry.StringVal(commands)),
		telemetry.Attr(cliusage.AttrInteractive, telemetry.BoolVal(interactive)),
	)
	return telemetry.LogRecord{TimeUnixNano: ts, Attributes: attrs}
}

func sessionEnd(ts, device, cliVersion, os, arch string, success, interactive bool, errorCategory string) telemetry.LogRecord {
	attrs := envelope(device, cliVersion, os, arch)
	attrs = append(attrs,
		telemetry.Attr(cliusage.AttrEventName, telemetry.StringVal(cliusage.EventSessionEnd)),
		telemetry.Attr(cliusage.AttrSuccess, telemetry.BoolVal(success)),
		telemetry.Attr(cliusage.AttrInteractive, telemetry.BoolVal(interactive)),
	)
	if !success {
		attrs = append(attrs, telemetry.Attr(cliusage.AttrErrorCategory, telemetry.StringVal(errorCategory)))
	}
	return telemetry.LogRecord{TimeUnixNano: ts, Attributes: attrs}
}

func counterMap(cs []Counter) map[[3]string]int64 {
	m := make(map[[3]string]int64, len(cs))
	for _, c := range cs {
		m[[3]string{c.Day, c.Dimension, c.Key}] = c.Count
	}
	return m
}

func deviceDaySet(dds []DeviceDay) map[DeviceDay]bool {
	m := make(map[DeviceDay]bool, len(dds))
	for _, dd := range dds {
		m[dd] = true
	}
	return m
}

// TestAccumulatorFold exercises the full fold across two days, several devices
// and both session events, asserting exact device-day pairs and counter values.
// The two batches are Add'd separately to also prove additivity across calls.
func TestAccumulatorFold(t *testing.T) {
	acc := testAccumulator()

	a := nanosAt(dayA)
	b := nanosAt(dayB)

	// Day A: device d1 succeeds (build,test), device d2 fails (deploy, api),
	// device d3 only starts (test). A non-session record on day A carrying
	// device d3 proves device-day is event-independent.
	acc.Add([]telemetry.ResourceLogs{cliResource(
		sessionStart(a, "d1", "build,test", true),
		sessionEnd(a, "d1", "1.2.3", "darwin", "arm64", true, true, ""),
		sessionStart(a, "d2", "deploy", false),
		sessionEnd(a, "d2", "1.2.3", "linux", "amd64", false, false, cliusage.ErrorCategoryAPI),
		sessionStart(a, "d3", "test", true),
	)})

	// Day B: device d1 succeeds (build). A record with no timestamp (device d4)
	// must be skipped. A non-CLI resource must be ignored wholesale.
	acc.Add([]telemetry.ResourceLogs{
		cliResource(
			sessionStart(b, "d1", "build", true),
			sessionEnd(b, "d1", "1.3.0", "darwin", "arm64", true, true, ""),
			// no timestamp -> skipped, so d4 never becomes a device-day.
			telemetry.LogRecord{Attributes: envelope("d4", "1.3.0", "darwin", "arm64")},
		),
		{ // non-CLI resource -> ignored
			Resource: telemetry.Resource{Attributes: []telemetry.KeyValue{
				telemetry.Attr(telemetry.AttrServiceName, telemetry.StringVal("some-other-service")),
			}},
			ScopeLogs: []telemetry.ScopeLogs{{LogRecords: []telemetry.LogRecord{
				sessionStart(b, "dX", "build", true),
			}}},
		},
	})

	snap := acc.Drain()

	// --- device-day set ---
	dds := deviceDaySet(snap.DeviceDays)
	wantDDs := []DeviceDay{
		{Day: "2026-07-25", DeviceID: "d1"},
		{Day: "2026-07-25", DeviceID: "d2"},
		{Day: "2026-07-25", DeviceID: "d3"},
		{Day: "2026-07-26", DeviceID: "d1"},
	}
	if len(snap.DeviceDays) != len(wantDDs) {
		t.Fatalf("device-days = %v, want exactly %v", snap.DeviceDays, wantDDs)
	}
	for _, dd := range wantDDs {
		if !dds[dd] {
			t.Errorf("missing device-day %+v", dd)
		}
	}
	if dds[(DeviceDay{Day: "2026-07-26", DeviceID: "d4"})] {
		t.Errorf("device d4 (no timestamp) must not be counted")
	}
	if dds[(DeviceDay{Day: "2026-07-26", DeviceID: "dX"})] {
		t.Errorf("device dX (non-CLI resource) must not be counted")
	}

	// --- counters ---
	cnt := counterMap(snap.Counters)
	want := map[[3]string]int64{
		// command mix (session:start only)
		{"2026-07-25", DimCommand, "build"}:  1,
		{"2026-07-25", DimCommand, "test"}:   2, // d1 "build,test" + d3 "test"
		{"2026-07-25", DimCommand, "deploy"}: 1,
		{"2026-07-26", DimCommand, "build"}:  1,
		// outcomes (session:end only)
		{"2026-07-25", DimOutcome, "success"}:   1,
		{"2026-07-25", DimOutcome, "error:api"}: 1,
		{"2026-07-26", DimOutcome, "success"}:   1,
		// cli_version / os / arch (session:end only; d3 has no end -> not counted)
		{"2026-07-25", DimCLIVersion, "1.2.3"}: 2,
		{"2026-07-26", DimCLIVersion, "1.3.0"}: 1,
		{"2026-07-25", DimOS, "darwin"}:        1,
		{"2026-07-25", DimOS, "linux"}:         1,
		{"2026-07-26", DimOS, "darwin"}:        1,
		{"2026-07-25", DimArch, "arm64"}:       1,
		{"2026-07-25", DimArch, "amd64"}:       1,
		{"2026-07-26", DimArch, "arm64"}:       1,
		// interactivity (session:end only)
		{"2026-07-25", DimInteractive, "true"}:  1,
		{"2026-07-25", DimInteractive, "false"}: 1,
		{"2026-07-26", DimInteractive, "true"}:  1,
	}
	for k, v := range want {
		if cnt[k] != v {
			t.Errorf("counter %v = %d, want %d", k, cnt[k], v)
		}
	}
	if len(cnt) != len(want) {
		t.Errorf("counter set size = %d, want %d\n got: %v", len(cnt), len(want), snap.Counters)
	}
}

// TestDrainResets proves Drain returns the accumulated state and leaves the
// accumulator empty (so a second flush window starts clean).
func TestDrainResets(t *testing.T) {
	acc := testAccumulator()
	acc.Add([]telemetry.ResourceLogs{cliResource(
		sessionStart(nanosAt(dayA), "d1", "build", true),
	)})
	if got := acc.Drain(); got.Empty() {
		t.Fatal("first drain should not be empty")
	}
	if got := acc.Drain(); !got.Empty() || got.ID != "" {
		t.Fatalf("second drain should be empty, got %+v", got)
	}
}

// TestConcurrentAddDrainConserves pins the concurrency invariant: Add and Drain
// hold the same mutex, so under interleaving no count is lost or double-counted.
// A writer emits N identical session:start records (each a "build" command on
// day A) while a concurrent drainer repeatedly drains; the sum of every drained
// "build" counter plus the final drain must equal exactly N. Run under -race in
// the gate, this also proves the two mutators never race.
func TestConcurrentAddDrainConserves(t *testing.T) {
	const n = 2000
	acc := testAccumulator()
	key := [3]string{"2026-07-25", DimCommand, "build"}

	var total int64
	drain := func(s Snapshot) { total += counterMap(s.Counters)[key] }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rec := []telemetry.ResourceLogs{cliResource(sessionStart(nanosAt(dayA), "d1", "build", true))}
		for i := 0; i < n; i++ {
			acc.Add(rec)
		}
	}()
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				drain(acc.Drain())
			}
		}
	}()

	// Let the writer finish, then stop the drainer and account for the tail.
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
	drain(acc.Drain())

	if total != n {
		t.Fatalf("conserved build count = %d, want %d", total, n)
	}
}

// TestFailedSessionWithoutSuccessMissing guards the outcome branch: a session
// with success absent contributes no outcome counter (defensive, though the
// sanitizer guarantees success is present on session:end).
func TestSessionEndMissingSuccessNoOutcome(t *testing.T) {
	acc := testAccumulator()
	rec := telemetry.LogRecord{
		TimeUnixNano: nanosAt(dayA),
		Attributes: append(envelope("d1", "1.0.0", "darwin", "arm64"),
			telemetry.Attr(cliusage.AttrEventName, telemetry.StringVal(cliusage.EventSessionEnd)),
			// no success attribute
		),
	}
	acc.Add([]telemetry.ResourceLogs{cliResource(rec)})
	snap := acc.Drain()
	for _, c := range snap.Counters {
		if c.Dimension == DimOutcome {
			t.Fatalf("no outcome counter expected when success is absent, got %+v", c)
		}
	}
	// device-day and envelope dimensions are still recorded.
	if len(snap.DeviceDays) != 1 {
		t.Fatalf("device-day should still be recorded, got %v", snap.DeviceDays)
	}
}

func TestAccumulatorCardinalityBoundsFailClosed(t *testing.T) {
	acc := testAccumulator()
	day := "2026-07-29"

	for i := 0; i < maxAccumulatorDeviceDays+1; i++ {
		acc.addDeviceDay(day, "device-"+strconv.Itoa(i))
	}
	if got := len(acc.deviceDays); got != maxAccumulatorDeviceDays {
		t.Fatalf("device-day cardinality = %d, want hard bound %d", got, maxAccumulatorDeviceDays)
	}

	for i := 0; i < counterDimensionCaps[DimCLIVersion]+1; i++ {
		acc.addCounter(day, DimCLIVersion, "1.0."+strconv.Itoa(i), "device", true)
	}
	if got := len(acc.counters); got != counterDimensionCaps[DimCLIVersion] {
		t.Fatalf("counter cardinality = %d, want dimension bound %d", got, counterDimensionCaps[DimCLIVersion])
	}

	contributorAcc := testAccumulator()
	// Use one counter key and unique devices to isolate the contributor ceiling.
	for i := 0; i < contributorDimensionCaps[DimOutcome]+1; i++ {
		contributorAcc.addCounter(day, DimOutcome, outcomeSuccess, "contributor-"+strconv.Itoa(i), true)
	}
	if got := len(contributorAcc.contributors); got != contributorDimensionCaps[DimOutcome] {
		t.Fatalf("contributor cardinality = %d, want dimension bound %d", got, contributorDimensionCaps[DimOutcome])
	}

	snap := acc.Drain()
	wantOverflows := []Overflow{
		{Day: day, Dimension: DimCLIVersion},
		{Day: day, Dimension: DimDevices},
	}
	if !reflect.DeepEqual(snap.Overflows, wantOverflows) {
		t.Fatalf("overflows = %v, want %v", snap.Overflows, wantOverflows)
	}
	if got := contributorAcc.Drain().Overflows; len(got) != 1 ||
		got[0] != (Overflow{Day: day, Dimension: DimOutcome}) {
		t.Fatalf("contributor overflows = %v, want outcome overflow on %s", got, day)
	}
}

func TestDimensionReservationsStayWithinGlobalCaps(t *testing.T) {
	var counters, contributors int
	for dimension, capacity := range counterDimensionCaps {
		if dimension == "*" || capacity <= 0 {
			t.Fatalf("invalid counter reservation %q=%d", dimension, capacity)
		}
		counters += capacity
	}
	for dimension, capacity := range contributorDimensionCaps {
		if dimension == "*" || capacity <= 0 {
			t.Fatalf("invalid contributor reservation %q=%d", dimension, capacity)
		}
		contributors += capacity
	}
	if counters > maxAccumulatorCounters {
		t.Fatalf("counter reservations total %d, global cap %d", counters, maxAccumulatorCounters)
	}
	if contributors > maxAccumulatorContributors {
		t.Fatalf("contributor reservations total %d, global cap %d", contributors, maxAccumulatorContributors)
	}
}

func TestPerDayReservationsDoNotCrossContaminate(t *testing.T) {
	acc := testAccumulator()
	commands := strings.Join(cliusage.Commands, ",")
	for _, fixture := range []struct {
		at     time.Time
		device string
	}{
		{at: dayA, device: "day-a"},
		{at: dayB, device: "day-b"},
	} {
		acc.Add([]telemetry.ResourceLogs{cliResource(
			sessionStart(nanosAt(fixture.at), fixture.device, commands, true),
		)})
	}
	snap := acc.Drain()
	if len(snap.Overflows) != 0 {
		t.Fatalf("valid command vocabulary on two days overflowed: %v", snap.Overflows)
	}
	var commandCounters int
	for _, counter := range snap.Counters {
		if counter.Dimension == DimCommand {
			commandCounters++
		}
	}
	if want := len(cliusage.Commands) * 2; commandCounters != want {
		t.Fatalf("command counters = %d, want %d across two independent days", commandCounters, want)
	}

	contributors := testAccumulator()
	const perDay = 6_000 // cumulative exceeds the old dimension-only 10k accounting
	for dayIndex, day := range []string{"2026-07-25", "2026-07-26"} {
		for i := 0; i < perDay; i++ {
			contributors.addCounter(
				day, DimOutcome, outcomeSuccess,
				"device-"+strconv.Itoa(dayIndex)+"-"+strconv.Itoa(i), true,
			)
		}
	}
	contributorSnap := contributors.Drain()
	if len(contributorSnap.Overflows) != 0 {
		t.Fatalf("contributors on independent days overflowed: %v", contributorSnap.Overflows)
	}
	if got, want := len(contributorSnap.Contributors), perDay*2; got != want {
		t.Fatalf("contributors = %d, want %d", got, want)
	}
}

func TestEachDayCapsIndependentlyAndDrainResetsReservations(t *testing.T) {
	acc := testAccumulator()
	for _, day := range []string{"2026-07-25", "2026-07-26"} {
		for i := 0; i <= counterDimensionCaps[DimCommand]; i++ {
			acc.addCounter(day, DimCommand, "command-"+strconv.Itoa(i), "", false)
		}
	}
	snap := acc.Drain()
	want := []Overflow{
		{Day: "2026-07-25", Dimension: DimCommand},
		{Day: "2026-07-26", Dimension: DimCommand},
	}
	if !reflect.DeepEqual(snap.Overflows, want) {
		t.Fatalf("per-day overflows = %v, want %v", snap.Overflows, want)
	}
	if len(acc.counterCounts) != 0 || len(acc.contributorCounts) != 0 {
		t.Fatalf("Drain retained reservations: counters=%v contributors=%v",
			acc.counterCounts, acc.contributorCounts)
	}

	for _, command := range cliusage.Commands {
		acc.addCounter("2026-07-26", DimCommand, command, "", false)
	}
	if reset := acc.Drain(); len(reset.Overflows) != 0 {
		t.Fatalf("fresh post-Drain reservation inherited overflow: %v", reset.Overflows)
	}
}

func TestGlobalSnapshotCapsRemainDimensionScoped(t *testing.T) {
	counters := testAccumulator()
	for i := 0; i < counterDimensionCaps[DimCLIVersion]; i++ {
		counters.addCounter("2026-07-25", DimCLIVersion, "a-"+strconv.Itoa(i), "", false)
	}
	for i := 0; i < maxAccumulatorCounters-counterDimensionCaps[DimCLIVersion]; i++ {
		counters.addCounter("2026-07-26", DimCLIVersion, "b-"+strconv.Itoa(i), "", false)
	}
	counters.addCounter("2026-07-26", DimCLIVersion, "global-overflow", "", false)
	counterSnap := counters.Drain()
	if len(counterSnap.Counters) != maxAccumulatorCounters {
		t.Fatalf("global counter cardinality = %d, want %d", len(counterSnap.Counters), maxAccumulatorCounters)
	}
	if got := counterSnap.Overflows; len(got) != 1 ||
		got[0] != (Overflow{Day: "2026-07-26", Dimension: DimCLIVersion}) {
		t.Fatalf("global counter overflow marker = %v", got)
	}

	contributors := testAccumulator()
	for dayIndex := 0; len(contributors.contributors) < maxAccumulatorContributors; dayIndex++ {
		day := "day-" + strconv.Itoa(dayIndex)
		reservation := dayDimensionKey{day: day, dim: DimOutcome}
		for i := 0; i < contributorDimensionCaps[DimOutcome] &&
			len(contributors.contributors) < maxAccumulatorContributors; i++ {
			key := contributorKey{
				day: day, dim: DimOutcome, key: outcomeSuccess,
				device: strconv.Itoa(dayIndex) + "-" + strconv.Itoa(i),
			}
			contributors.contributors[key] = struct{}{}
			contributors.contributorCounts[reservation]++
		}
	}
	contributors.addCounter("2026-07-29", DimCommand, "build", "next-device", true)
	contributorSnap := contributors.Drain()
	if len(contributorSnap.Contributors) != maxAccumulatorContributors {
		t.Fatalf("global contributor cardinality = %d, want %d",
			len(contributorSnap.Contributors), maxAccumulatorContributors)
	}
	if got := contributorSnap.Overflows; len(got) != 1 ||
		got[0] != (Overflow{Day: "2026-07-29", Dimension: DimCommand}) {
		t.Fatalf("global contributor overflow marker = %v", got)
	}
}

func TestAccumulatorTracksDistinctContributorsNotEventVolume(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "small-groups-are-withheld", "the-write-side-counts-distinct-contributors-not-events")
	acc := testAccumulator()
	ts := nanosAt(dayA)
	for i := 0; i < 5; i++ {
		acc.Add([]telemetry.ResourceLogs{cliResource(
			sessionEnd(ts, "same-device", "1.2.3", "darwin", "arm64", true, true, ""),
		)})
	}
	snap := acc.Drain()
	var outcomeContributors int
	for _, c := range snap.Contributors {
		if c.Dimension == DimOutcome && c.Key == outcomeSuccess {
			outcomeContributors++
		}
	}
	if outcomeContributors != 1 {
		t.Fatalf("outcome contributors = %d, want one distinct device", outcomeContributors)
	}
	if got := counterMap(snap.Counters)[[3]string{"2026-07-25", DimOutcome, outcomeSuccess}]; got != 5 {
		t.Fatalf("event tally = %d, want all five sessions preserved", got)
	}
}

func TestAnonymousVocabularyCannotCreateGlobalOverflow(t *testing.T) {
	acc := testAccumulator()
	const today = "2026-07-29"

	// Exhaust the only intentionally open projected vocabulary.
	for i := 0; i <= counterDimensionCaps[DimCLIVersion]; i++ {
		acc.addCounter(today, DimCLIVersion, "1.0."+strconv.Itoa(i), "device", true)
	}
	// Grammar-valid attacker commands bypassing the sanitizer are ignored by
	// the accumulator's defense-in-depth check and cannot create keys.
	for i := 0; i < 10_000; i++ {
		acc.Add([]telemetry.ResourceLogs{cliResource(
			sessionStart(nanosAt(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)),
				"device-"+strconv.Itoa(i), "attacker-"+strconv.Itoa(i), true),
		)})
	}

	if len(acc.overflows) > maxAccumulatorOverflows {
		t.Fatalf("overflow tracking grew to %d entries, bound is %d", len(acc.overflows), maxAccumulatorOverflows)
	}
	snap := acc.Drain()
	if len(snap.Overflows) != 1 || snap.Overflows[0] != (Overflow{Day: today, Dimension: DimCLIVersion}) {
		t.Fatalf("overflows = %v, want only cli-version unavailable", snap.Overflows)
	}
	for _, counter := range snap.Counters {
		if counter.Dimension == DimCommand && strings.HasPrefix(counter.Key, "attacker-") {
			t.Fatalf("attacker command reached aggregate projection: %+v", counter)
		}
	}
	if err := Flush(context.Background(), nil, snap); err != nil {
		t.Fatalf("bounded fail-closed snapshot was rejected: %v", err)
	}
}
