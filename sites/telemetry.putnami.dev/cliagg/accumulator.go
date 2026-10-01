// Package cliagg turns the sanitized CLI-usage records the receiver already
// produces into durable daily aggregates. It is an emit-time
// tap: the receiver tees every accepted batch into an in-memory Accumulator,
// and a background flush upserts the folded aggregates into Postgres. There is
// no log read-back and the receiver never persists raw records. Durable state
// is limited to aggregate counts, short-lived contributor membership used for
// privacy suppression, and bounded operational ledgers.
//
// The fold reads every value through the shared
// go.putnami.dev/protocol/telemetry/cliusage vocabulary, so the aggregation
// keys cannot drift from the producer (the CLI's data-minimization guard) or
// the sanitizer.
package cliagg

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"
)

// Counter dimensions. These name the aggregation buckets this receiver rolls
// sessions into; they are the receiver's own storage vocabulary, not a wire
// contract, so they are deliberately NOT in cliusage. The attribute KEYS whose
// values feed each bucket all come from cliusage, which is what keeps the fold
// pinned to the producer.
const (
	DimCommand     = "command"
	DimOutcome     = "outcome"
	DimCLIVersion  = "cli_version"
	DimOS          = "os"
	DimArch        = "arch"
	DimInteractive = "interactive"
	DimProjects    = "projects"
	DimJobs        = "jobs"
	DimDuration    = "duration"
	DimFlag        = "flag"
	DimDevices     = "devices"
)

// The accumulator is deliberately smaller than the durable per-day ceilings:
// one process can never turn a single flush into unbounded memory or DB work.
const (
	maxAccumulatorDeviceDays   = 10_000
	maxAccumulatorCounters     = 2_004
	maxAccumulatorContributors = 74_000
	retentionPastDays          = 34
	retentionFutureDays        = 0
	maxAccumulatorOverflows    = (retentionPastDays + retentionFutureDays + 1) * 7
)

var counterDimensionCaps = map[string]int{
	DimCommand: 16, DimOutcome: 8, DimCLIVersion: 1_880,
	DimOS: 32, DimArch: 32, DimInteractive: 4,
	DimProjects: 8, DimJobs: 8, DimDuration: 8, DimFlag: 8,
}

var contributorDimensionCaps = map[string]int{
	DimCommand: 10_000, DimOutcome: 10_000, DimCLIVersion: 8_000,
	DimOS: 8_000, DimArch: 8_000, DimInteractive: 6_000,
	DimProjects: 6_000, DimJobs: 6_000, DimDuration: 6_000, DimFlag: 6_000,
}

// Size ranges. Project and job counts and session durations are folded into
// fixed ranges, never stored as exact values: a range key is a closed
// vocabulary, and an exact count or millisecond value would be a fingerprint.
// Each list is ordered by lower bound; a value falls into the last range whose
// lower bound it reaches.
type sizeRange struct {
	min int64
	key string
}

var (
	projectRanges = []sizeRange{
		{0, "0"}, {1, "1"}, {2, "2-5"}, {6, "6-20"},
		{21, "21-50"}, {51, "51-100"}, {101, "101-500"}, {501, "501+"},
	}
	jobRanges = []sizeRange{
		{0, "0"}, {1, "1"}, {2, "2-5"}, {6, "6-20"},
		{21, "21-100"}, {101, "101-500"}, {501, "501-2000"}, {2001, "2001+"},
	}
	durationRanges = []sizeRange{
		{0, "0-1s"}, {1_000, "1-5s"}, {5_000, "5-30s"}, {30_000, "30s-2m"},
		{120_000, "2-10m"}, {600_000, "10-30m"}, {1_800_000, "30m+"},
	}
)

// Flag keys: the presence-only flags a session:start reports, keyed by the
// flag name without its "flag." prefix. A flag counts once per session that
// set it; an absent or false flag adds nothing.
var flagKeys = []struct{ attr, key string }{
	{cliusage.AttrFlagImpacted, "impacted"},
	{cliusage.AttrFlagCoverage, "coverage"},
	{cliusage.AttrFlagOutput, "output"},
	{cliusage.AttrFlagNoCache, "no-cache"},
	{cliusage.AttrFlagProjects, "projects"},
	{cliusage.AttrFlagWatch, "watch"},
}

// Outcome keys: a successful session is "success"; a failed one is
// "error:<category>" so the fixed cliusage error categories stay legible in the
// counter key.
const (
	outcomeSuccess     = "success"
	outcomeErrorPrefix = "error:"
)

// DeviceDay is one (UTC day, device id) observation — the unit daily and
// monthly unique-device counts are computed from.
type DeviceDay struct {
	Day      string // YYYY-MM-DD (UTC)
	DeviceID string
}

// Counter is one accumulated (day, dimension, key) tally.
type Counter struct {
	Day       string
	Dimension string
	Key       string
	Count     int64
}

// Contributor is one distinct rotating device's membership in a counter
// cohort. Counts remain event totals; this projection exists only to decide
// whether at least k different contributors are represented.
type Contributor struct {
	Day       string
	Dimension string
	Key       string
	DeviceID  string
}

// Overflow identifies a day and dimension whose cardinality exceeded the
// accumulator's bounded admission capacity.
type Overflow struct {
	Day       string
	Dimension string
}

// Snapshot is a drained view of the accumulator: the device-day set and the
// counter tallies at drain time. Both slices are sorted deterministically so a
// flush (and its tests) see a stable order.
type Snapshot struct {
	ID           string
	DeviceDays   []DeviceDay
	Counters     []Counter
	Contributors []Contributor
	Overflows    []Overflow
}

// Empty reports whether the snapshot carries nothing to persist.
func (s Snapshot) Empty() bool {
	return len(s.DeviceDays) == 0 && len(s.Counters) == 0 &&
		len(s.Contributors) == 0 && len(s.Overflows) == 0
}

type deviceDayKey struct{ day, device string }
type counterKey struct{ day, dim, key string }
type contributorKey struct{ day, dim, key, device string }
type dayDimensionKey struct{ day, dim string }
type overflowKey struct{ day, dim string }

// Accumulator folds []telemetry.ResourceLogs into an in-memory device-day set
// and counter tallies. It is safe for concurrent use: Add and Drain are the
// only mutators and both hold mu, so the emit tap and the periodic flush never
// race.
type Accumulator struct {
	mu                sync.Mutex
	deviceDays        map[deviceDayKey]struct{}
	counters          map[counterKey]int64
	contributors      map[contributorKey]struct{}
	counterCounts     map[dayDimensionKey]int
	contributorCounts map[dayDimensionKey]int
	overflows         map[overflowKey]struct{}
	now               func() time.Time
}

// NewAccumulator returns an empty Accumulator.
func NewAccumulator() *Accumulator {
	return newAccumulator(time.Now)
}

func newAccumulator(now func() time.Time) *Accumulator {
	if now == nil {
		now = time.Now
	}
	return &Accumulator{
		deviceDays:        make(map[deviceDayKey]struct{}),
		counters:          make(map[counterKey]int64),
		contributors:      make(map[contributorKey]struct{}),
		counterCounts:     make(map[dayDimensionKey]int),
		contributorCounts: make(map[dayDimensionKey]int),
		overflows:         make(map[overflowKey]struct{}),
		now:               now,
	}
}

// Add folds one batch of sanitized resource logs into the accumulator. A
// resource that does not identify service.name=putnami-cli is ignored, as is
// any record without a valid timestamp. Add never blocks on I/O.
func (a *Accumulator) Add(resourceLogs []telemetry.ResourceLogs) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rl := range resourceLogs {
		if !resourceIsCLIUsage(rl.Resource) {
			continue
		}
		for _, sl := range rl.ScopeLogs {
			for _, rec := range sl.LogRecords {
				a.addRecord(rec)
			}
		}
	}
}

// addRecord folds a single record. The caller holds a.mu.
func (a *Accumulator) addRecord(rec telemetry.LogRecord) {
	day, ok := dayOf(rec.TimeUnixNano)
	if !ok {
		return // skip records with no valid timestamp
	}
	if !dayWithinRetention(day, a.now()) {
		return
	}
	attrs := indexAttrs(rec.Attributes)

	// Device-day set: every record carrying a device.id, regardless of event.
	id, hasDevice := stringAttr(attrs, cliusage.AttrDeviceID)
	if hasDevice && id != "" {
		a.addDeviceDay(day, id)
	}

	event, _ := stringAttr(attrs, cliusage.AttrEventName)
	switch event {
	case cliusage.EventSessionStart:
		// Command mix: split the comma-joined command list and tally each.
		if commands, ok := stringAttr(attrs, cliusage.AttrCommands); ok {
			for _, cmd := range strings.Split(commands, ",") {
				if cmd = strings.TrimSpace(cmd); cliusage.IsCommand(cmd) {
					a.addCounter(day, DimCommand, cmd, id, hasDevice)
				}
			}
		}
		// Run shape: how many projects and jobs the run planned, and which
		// presence-only flags it set. Tallied on session:start, the event that
		// carries them, so each session counts once per range.
		if n, ok := intAttr(attrs, cliusage.AttrProjects); ok {
			a.addCounter(day, DimProjects, rangeKey(projectRanges, n), id, hasDevice)
		}
		if n, ok := intAttr(attrs, cliusage.AttrJobs); ok {
			a.addCounter(day, DimJobs, rangeKey(jobRanges, n), id, hasDevice)
		}
		for _, flag := range flagKeys {
			if set, ok := boolAttr(attrs, flag.attr); ok && set {
				a.addCounter(day, DimFlag, flag.key, id, hasDevice)
			}
		}
	case cliusage.EventSessionEnd:
		// Outcome, envelope, and interactivity are tallied on session:end only,
		// so each session contributes exactly one count per dimension (the paired
		// session:start would otherwise double-count the envelope dimensions).
		if success, ok := boolAttr(attrs, cliusage.AttrSuccess); ok {
			outcome := outcomeSuccess
			if !success {
				cat, _ := stringAttr(attrs, cliusage.AttrErrorCategory)
				outcome = outcomeErrorPrefix + cat
			}
			a.addCounter(day, DimOutcome, outcome, id, hasDevice)
		}
		if v, ok := stringAttr(attrs, cliusage.AttrCLIVersion); ok {
			a.addCounter(day, DimCLIVersion, v, id, hasDevice)
		}
		if v, ok := stringAttr(attrs, cliusage.AttrOS); ok {
			a.addCounter(day, DimOS, v, id, hasDevice)
		}
		if v, ok := stringAttr(attrs, cliusage.AttrArch); ok {
			a.addCounter(day, DimArch, v, id, hasDevice)
		}
		if interactive, ok := boolAttr(attrs, cliusage.AttrInteractive); ok {
			a.addCounter(day, DimInteractive, strconv.FormatBool(interactive), id, hasDevice)
		}
		if ms, ok := intAttr(attrs, cliusage.AttrDuration); ok {
			a.addCounter(day, DimDuration, rangeKey(durationRanges, ms), id, hasDevice)
		}
	}
}

// rangeKey returns the key of the range n falls into. A negative value is
// clamped into the first range: the sanitizer accepts any int64, and dropping
// the value would make the range totals disagree with the session count.
func rangeKey(ranges []sizeRange, n int64) string {
	key := ranges[0].key
	for _, r := range ranges {
		if n < r.min {
			break
		}
		key = r.key
	}
	return key
}

func (a *Accumulator) addDeviceDay(day, device string) {
	k := deviceDayKey{day: day, device: device}
	if _, exists := a.deviceDays[k]; exists {
		return
	}
	if len(a.deviceDays) >= maxAccumulatorDeviceDays {
		a.markOverflow(day, DimDevices)
		return
	}
	a.deviceDays[k] = struct{}{}
}

func (a *Accumulator) addCounter(day, dim, key, device string, hasDevice bool) {
	k := counterKey{day: day, dim: dim, key: key}
	reservation := dayDimensionKey{day: day, dim: dim}
	if _, exists := a.counters[k]; !exists {
		capacity := counterDimensionCaps[dim]
		if capacity == 0 || a.counterCounts[reservation] >= capacity ||
			len(a.counters) >= maxAccumulatorCounters {
			a.markOverflow(day, dim)
			return
		}
		a.counterCounts[reservation]++
	}
	a.counters[k]++
	if !hasDevice || device == "" {
		return
	}
	ck := contributorKey{day: day, dim: dim, key: key, device: device}
	if _, exists := a.contributors[ck]; exists {
		return
	}
	capacity := contributorDimensionCaps[dim]
	if capacity == 0 || a.contributorCounts[reservation] >= capacity ||
		len(a.contributors) >= maxAccumulatorContributors {
		a.markOverflow(day, dim)
		return
	}
	a.contributors[ck] = struct{}{}
	a.contributorCounts[reservation]++
}

func (a *Accumulator) markOverflow(day, dimension string) {
	key := overflowKey{day: day, dim: dimension}
	if _, exists := a.overflows[key]; exists {
		return
	}
	if len(a.overflows) < maxAccumulatorOverflows {
		a.overflows[key] = struct{}{}
		return
	}
}

// Drain returns a deterministic snapshot of the accumulated state and resets
// the accumulator under the lock, so a concurrent Add starts a fresh window.
// The flush persists the returned snapshot; on a DB error it is dropped (the
// accumulator has already moved on — best-effort, no retry).
func (a *Accumulator) Drain() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	snap := a.snapshotLocked()
	a.deviceDays = make(map[deviceDayKey]struct{})
	a.counters = make(map[counterKey]int64)
	a.contributors = make(map[contributorKey]struct{})
	a.counterCounts = make(map[dayDimensionKey]int)
	a.contributorCounts = make(map[dayDimensionKey]int)
	a.overflows = make(map[overflowKey]struct{})
	return snap
}

func (a *Accumulator) snapshotLocked() Snapshot {
	dds := make([]DeviceDay, 0, len(a.deviceDays))
	for k := range a.deviceDays {
		dds = append(dds, DeviceDay{Day: k.day, DeviceID: k.device})
	}
	sort.Slice(dds, func(i, j int) bool {
		if dds[i].Day != dds[j].Day {
			return dds[i].Day < dds[j].Day
		}
		return dds[i].DeviceID < dds[j].DeviceID
	})

	cs := make([]Counter, 0, len(a.counters))
	for k, v := range a.counters {
		cs = append(cs, Counter{Day: k.day, Dimension: k.dim, Key: k.key, Count: v})
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Day != cs[j].Day {
			return cs[i].Day < cs[j].Day
		}
		if cs[i].Dimension != cs[j].Dimension {
			return cs[i].Dimension < cs[j].Dimension
		}
		return cs[i].Key < cs[j].Key
	})
	contributors := make([]Contributor, 0, len(a.contributors))
	for k := range a.contributors {
		contributors = append(contributors, Contributor{
			Day: k.day, Dimension: k.dim, Key: k.key, DeviceID: k.device,
		})
	}
	sort.Slice(contributors, func(i, j int) bool {
		a, b := contributors[i], contributors[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if a.Dimension != b.Dimension {
			return a.Dimension < b.Dimension
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.DeviceID < b.DeviceID
	})
	overflows := make([]Overflow, 0, len(a.overflows))
	for key := range a.overflows {
		overflows = append(overflows, Overflow{Day: key.day, Dimension: key.dim})
	}
	sort.Slice(overflows, func(i, j int) bool {
		if overflows[i].Day != overflows[j].Day {
			return overflows[i].Day < overflows[j].Day
		}
		return overflows[i].Dimension < overflows[j].Dimension
	})
	snap := Snapshot{
		DeviceDays: dds, Counters: cs,
		Contributors: contributors, Overflows: overflows,
	}
	if !snap.Empty() {
		snap.ID = newSnapshotID()
	}
	return snap
}

func newSnapshotID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		// An empty ID disables replay deduplication but does not weaken privacy;
		// crypto/rand failure is not allowed to break the fail-silent receiver.
		return ""
	}
	return hex.EncodeToString(id[:])
}

// dayOf derives the UTC calendar day from an OTLP unix-nano timestamp string.
// It reports ok=false for an empty, non-numeric, or non-positive value so the
// caller skips a record with no valid timestamp.
func dayOf(timeUnixNano string) (string, bool) {
	s := strings.TrimSpace(timeUnixNano)
	if s == "" {
		return "", false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return "", false
	}
	return time.Unix(n/1_000_000_000, n%1_000_000_000).UTC().Format("2006-01-02"), true
}

func dayWithinRetention(day string, now time.Time) bool {
	parsed, err := time.Parse(dayLayout, day)
	if err != nil {
		return false
	}
	utc := now.UTC()
	today := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return !parsed.Before(today.AddDate(0, 0, -retentionPastDays)) &&
		!parsed.After(today.AddDate(0, 0, retentionFutureDays))
}

// resourceIsCLIUsage reports whether the resource declares
// service.name=putnami-cli. The receiver only aggregates CLI-usage telemetry;
// anything else is ignored (the sanitizer already enforces this on the emit
// path, but the accumulator re-checks so it is correct on any input).
func resourceIsCLIUsage(res telemetry.Resource) bool {
	for _, kv := range res.Attributes {
		if kv.Key == telemetry.AttrServiceName {
			return kv.Value.StringValue != nil && *kv.Value.StringValue == cliusage.ServiceName
		}
	}
	return false
}

// indexAttrs builds a key→value view of a record's attributes, keeping the
// first occurrence of a duplicated key (matching the sanitizer's dedupe).
func indexAttrs(attrs []telemetry.KeyValue) map[string]telemetry.AnyValue {
	m := make(map[string]telemetry.AnyValue, len(attrs))
	for _, kv := range attrs {
		if _, dup := m[kv.Key]; dup {
			continue
		}
		m[kv.Key] = kv.Value
	}
	return m
}

func stringAttr(m map[string]telemetry.AnyValue, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v.StringValue == nil {
		return "", false
	}
	return *v.StringValue, true
}

func intAttr(m map[string]telemetry.AnyValue, key string) (int64, bool) {
	v, ok := m[key]
	if !ok || v.IntValue == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(*v.IntValue), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func boolAttr(m map[string]telemetry.AnyValue, key string) (bool, bool) {
	v, ok := m[key]
	if !ok || v.BoolValue == nil {
		return false, false
	}
	return *v.BoolValue, true
}
