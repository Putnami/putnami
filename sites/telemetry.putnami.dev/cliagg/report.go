package cliagg

// Aggregate read contract.
//
// This file is the whole privacy boundary of the read side. It turns the
// persisted S2c projection (cli_device_day, cli_daily_counter) into a bounded,
// typed report: fixed UTC calendar windows, fixed dimensions, explicit
// available/unavailable/suppressed states, and k-anonymity suppression applied
// BEFORE anything leaves the workload.
//
// What is deliberately NOT published, even though it is persisted:
//
//   - any device id, device set, or per-device lookup (only counts of DISTINCT
//     device ids inside the requested window),
//   - the full day × dimension × key cube (only per-window breakdowns and a
//     per-day session/device series), because the cross-tab of two published
//     partitions reconstructs cohorts the individual partitions suppress,
//   - any caller-selected filter, datasource, or projection.

import (
	stderrors "errors"
	"sort"
	"strings"
	"time"
)

// SuppressionThreshold is the k of the k-anonymity rule the read contract
// enforces at the workload boundary: no nonzero count below it is ever
// published, on any surface — summary, daily series, or breakdown.
const SuppressionThreshold int64 = 5

// Field states. Every count in the report carries one, so a consumer can never
// mistake "withheld" or "not measured" for zero.
const (
	// StateAvailable means Count.Value is an authoritative persisted number.
	StateAvailable = "available"
	// StateUnavailable means no authoritative counter exists for the field. It
	// is never inferred as zero.
	StateUnavailable = "unavailable"
	// StateSuppressed means the number exists but is withheld by the privacy
	// rule (a cohort below the threshold, or a value that would let one be
	// reconstructed by differencing).
	StateSuppressed = "suppressed"
)

// Reasons a count is not available. They are fixed enum values, never free text.
const (
	// ReasonBelowThreshold marks a cohort withheld by the k-anonymity rule.
	ReasonBelowThreshold = "below-threshold"
	// ReasonNotPersisted marks a field with no authoritative persisted counter
	// (the receiver keeps no authoritative ingest-health counter).
	ReasonNotPersisted = "no-persisted-counter"
	// ReasonNoPublishedData marks freshness when no day in the window has any
	// publishable data.
	ReasonNoPublishedData = "no-published-data"
	// ReasonProjectionOverflow marks a dimension whose bounded write projection
	// exhausted its reserved capacity. Other dimensions remain authoritative.
	ReasonProjectionOverflow = "projection-overflow"
)

// Accepted window specs. They are fixed UTC calendar windows ending with the
// current (still filling) UTC day; nothing else is accepted.
const (
	// Window1d is the current UTC day.
	Window1d = "1d"
	// Window7d is the current UTC day and the six days before it.
	Window7d = "7d"
	// Window30d is the current UTC day and the 29 days before it.
	Window30d = "30d"
	// DefaultWindow is used when a request omits the window entirely.
	DefaultWindow = Window7d
)

// ErrUnsupportedWindow is returned by ParseWindow for any spec outside the
// fixed set. The read surface has no other window vocabulary.
var ErrUnsupportedWindow = stderrors.New("unsupported window")

const dayLayout = "2006-01-02"

// maxBucketsPerDimension bounds a breakdown so the response size is a function
// of the contract, not of the data. Buckets beyond it (the smallest ones) fold
// into the breakdown's residual.
const maxBucketsPerDimension = 50

// maxKeyLength bounds a published bucket key. The write side only ever stores
// values from the closed cliusage vocabulary, so this is defense in depth: an
// unexpected key is folded into the residual rather than echoed.
const maxKeyLength = 64

// reportDimensions is the fixed, ordered breakdown vocabulary of the read
// contract. It is built from the Dim* constants the write side uses, so the read
// side cannot drift from what is actually persisted.
//
// The run-shape dimensions (projects, jobs, duration, flag) are appended after
// the original six so a reader that pins the original order still finds it as a
// prefix.
var reportDimensions = []string{
	DimCommand, DimOutcome, DimCLIVersion, DimOS, DimArch, DimInteractive,
	DimProjects, DimJobs, DimDuration, DimFlag,
}

// Windows returns the accepted window specs, in increasing length.
func Windows() []string { return []string{Window1d, Window7d, Window30d} }

// Dimensions returns the fixed breakdown vocabulary the report publishes.
func Dimensions() []string { return append([]string(nil), reportDimensions...) }

// Count is one number in the report together with its explicit state. Value is
// present only when State is StateAvailable, so an absent number can never be
// read as zero.
type Count struct {
	State  string `json:"state"`
	Value  *int64 `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Available reports whether the count carries an authoritative value.
func (c Count) Available() bool { return c.State == StateAvailable && c.Value != nil }

func available(v int64) Count { return Count{State: StateAvailable, Value: &v} }

func suppressedCount() Count {
	return Count{State: StateSuppressed, Reason: ReasonBelowThreshold}
}

func unavailableCount(reason string) Count {
	return Count{State: StateUnavailable, Reason: reason}
}

// publishCount applies the primary suppression rule to a standalone count:
// zero is published (an absence is not a cohort), anything below the threshold
// is withheld.
func publishCount(v int64) Count {
	if v == 0 || v >= SuppressionThreshold {
		return available(v)
	}
	return suppressedCount()
}

// Window is a resolved, fixed UTC calendar window. Bounds are calendar days:
// Start is inclusive, End is exclusive.
type Window struct {
	// Spec is the requested window ("1d", "7d", "30d").
	Spec string `json:"spec"`
	// Start is the first UTC day in the window (inclusive, YYYY-MM-DD).
	Start string `json:"start"`
	// End is the first UTC day after the window (exclusive, YYYY-MM-DD).
	End string `json:"end"`
	// Days is the number of daily buckets in the window.
	Days int `json:"days"`
	// PartialBucket reports that the last bucket is the current UTC day and is
	// therefore still filling.
	PartialBucket bool `json:"partialBucket"`
	// CompleteThrough is the last UTC day in the window known to be complete.
	// It is empty for the 1d window, whose only bucket is the current day.
	CompleteThrough string `json:"completeThrough,omitempty"`
}

// ParseWindow resolves a window spec against the UTC calendar day containing
// now. Any spec outside Windows() is rejected with ErrUnsupportedWindow — there
// is no free-form range, offset, or cursor.
func ParseWindow(spec string, now time.Time) (Window, error) {
	var days int
	switch spec {
	case Window1d:
		days = 1
	case Window7d:
		days = 7
	case Window30d:
		days = 30
	default:
		return Window{}, ErrUnsupportedWindow
	}

	utc := now.UTC()
	today := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	end := today.AddDate(0, 0, 1)
	start := end.AddDate(0, 0, -days)

	w := Window{
		Spec:          spec,
		Start:         start.Format(dayLayout),
		End:           end.Format(dayLayout),
		Days:          days,
		PartialBucket: true,
	}
	if days > 1 {
		w.CompleteThrough = today.AddDate(0, 0, -1).Format(dayLayout)
	}
	return w, nil
}

// days lists every UTC day in the window, in order.
func (w Window) days() []string {
	start, err := time.Parse(dayLayout, w.Start)
	if err != nil {
		return nil
	}
	out := make([]string, 0, w.Days)
	for i := 0; i < w.Days; i++ {
		out = append(out, start.AddDate(0, 0, i).Format(dayLayout))
	}
	return out
}

// Bucket is one published cohort of a breakdown. It only ever exists for a
// cohort at or above the suppression threshold.
type Bucket struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// Breakdown is the bounded per-dimension distribution over the window.
type Breakdown struct {
	// Dimension is one of Dimensions().
	Dimension string `json:"dimension"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	// Buckets holds the published cohorts, largest first. Cohorts below the
	// suppression threshold are never listed.
	Buckets []Bucket `json:"buckets"`
	// Other is the residual over every withheld cohort. It is published only
	// when doing so cannot reconstruct a withheld cohort by differencing.
	Other Count `json:"other"`
}

// DailyPoint is one UTC day of the series.
type DailyPoint struct {
	Day string `json:"day"`
	// Partial reports that this bucket is the current UTC day, still filling.
	Partial bool `json:"partial"`
	// Devices is the number of distinct device ids seen that day.
	Devices Count `json:"devices"`
	// Sessions is the number of sessions with a recorded outcome that day.
	Sessions Count `json:"sessions"`
}

// IngestHealth is the receiver's own ingest health. The receiver persists
// no authoritative ingest counter is persisted, so every field
// here is StateUnavailable. It is reported explicitly rather than omitted so a
// consumer never infers zero.
type IngestHealth struct {
	AcceptedRecords Count `json:"acceptedRecords"`
	RejectedRecords Count `json:"rejectedRecords"`
}

// Summary is the window-level roll-up.
type Summary struct {
	// Devices is the number of DISTINCT rotating device ids observed inside the
	// window. It is a count only: the contract exposes no identifier, set, or
	// lookup.
	Devices Count `json:"devices"`
	// Sessions is the number of sessions with a recorded outcome in the window.
	Sessions Count `json:"sessions"`
	// Commands is the number of command invocations in the window.
	Commands Count `json:"commands"`
	// Ingest is always unavailable; see IngestHealth.
	Ingest IngestHealth `json:"ingest"`
}

// Freshness reports how current the projection is. LatestDay is only ever a day
// whose own data is published, so freshness cannot disclose a suppressed day.
type Freshness struct {
	GeneratedAt string `json:"generatedAt"`
	State       string `json:"state"`
	LatestDay   string `json:"latestDay,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// Report is the complete aggregate read contract response.
type Report struct {
	Window               Window       `json:"window"`
	SuppressionThreshold int64        `json:"suppressionThreshold"`
	Freshness            Freshness    `json:"freshness"`
	Summary              Summary      `json:"summary"`
	Daily                []DailyPoint `json:"daily"`
	Breakdowns           []Breakdown  `json:"breakdowns"`
}

// raw is the exact set of persisted facts a report is built from. It holds
// counts only — never a device id, an IP, a raw event, or free text from a
// request.
type raw struct {
	devices                  int64            // distinct device ids inside the window
	dailyDevices             map[string]int64 // UTC day → distinct device ids that day
	dailySessions            map[string]int64 // UTC day → session count
	dailySessionContributors map[string]int64
	// counters is dimension → key → event total, restricted to reportDimensions.
	counters              map[string]map[string]int64
	counterContributors   map[string]map[string]int64
	dimensionContributors map[string]int64
	unavailable           map[string]bool
}

// cohort is one (key, count) pair of a partition.
type cohort struct {
	key          string
	count        int64
	contributors int64
}

// partition is the suppression outcome for one set of cohorts that together sum
// to a single total.
//
// The rule has two halves:
//
//   - Primary suppression: a cohort below the threshold is never published.
//     (A cohort with an unpublishable key, or one past the cardinality bound, is
//     withheld too — both are already at or above the threshold, so they only
//     ever make the residual safer.)
//   - Residual publication: the residual (the sum of everything withheld) and
//     the partition total are publishable only when the residual is 0 or at
//     least the threshold. Since the size rule only ever withholds cohorts below
//     the threshold, a residual made of small cohorts reaches it only by
//     spanning two or more of them, so publishing it discloses no single small
//     cohort.
//
// Deliberately NOT done here: complementary suppression that withholds
// already-publishable cohorts to force a safe residual. Withholding the total
// instead keeps the large cohorts visible, and is equally safe — the adversary
// learns the sum of the visible cohorts, which they were entitled to anyway.
type partition struct {
	visible      []Bucket
	residual     int64
	hidden       int
	total        int64
	unsafeHidden bool
	// safe reports that the residual (and therefore the total) may be published
	// without exposing a withheld cohort.
	safe bool
}

// suppress applies the rule above to one set of cohorts. maxBuckets bounds how
// many cohorts are published; the smallest ones beyond it fold into the
// residual, which keeps the response size a function of the contract.
func suppress(cohorts []cohort, maxBuckets int) partition {
	sorted := make([]cohort, len(cohorts))
	copy(sorted, cohorts)
	// Ascending by count, then key: the withholding order is deterministic.
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].count != sorted[j].count {
			return sorted[i].count < sorted[j].count
		}
		return sorted[i].key < sorted[j].key
	})

	p := partition{}
	kept := make([]cohort, 0, len(sorted))
	for _, c := range sorted {
		if c.count <= 0 {
			continue // an empty tally is not a cohort; it discloses nothing
		}
		p.total += c.count
		if c.contributors < SuppressionThreshold || !publishableKey(c.key) {
			p.residual += c.count
			p.hidden++
			if c.contributors < SuppressionThreshold {
				p.unsafeHidden = true
			}
			continue
		}
		kept = append(kept, c)
	}

	// Cardinality bound: fold the smallest publishable cohorts into the residual
	// until the bucket count fits. Each folded cohort is itself at or above the
	// threshold, so the residual only grows past it.
	for maxBuckets > 0 && len(kept) > maxBuckets {
		p.residual += kept[0].count
		p.hidden++
		kept = kept[1:]
	}

	p.visible = make([]Bucket, 0, len(kept))
	for _, c := range kept {
		p.visible = append(p.visible, Bucket{Key: c.key, Count: c.count})
	}
	// Present largest first, ties by key, so the response is deterministic.
	sort.Slice(p.visible, func(i, j int) bool {
		if p.visible[i].Count != p.visible[j].Count {
			return p.visible[i].Count > p.visible[j].Count
		}
		return p.visible[i].Key < p.visible[j].Key
	})

	// Without projecting contributor sets into the read process, the union of
	// hidden cohorts is unknown: one device may contribute to many buckets.
	// Therefore residuals/totals are fail-closed whenever anything is hidden.
	p.safe = !p.unsafeHidden
	return p
}

// publishableKey reports whether a persisted key may be echoed. The write side
// only stores closed-vocabulary values (command names, an outcome, a CLI
// version, an OS/arch, a boolean), so this is a defensive bound: anything
// unexpectedly long, outside the conservative charset — no spaces, no slashes,
// nothing that could be a path or a sentence — or shaped like an IP address is
// folded into the residual instead of crossing the API boundary.
func publishableKey(key string) bool {
	if key == "" || len(key) > maxKeyLength {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '+' || r == ':' || r == '~':
		default:
			return false
		}
	}
	return !looksLikeAddress(key)
}

// looksLikeAddress reports whether a key has the shape of an IPv4 dotted quad or
// an IPv6 address. No closed-vocabulary value has that shape (a CLI version is a
// three-part semver, an outcome carries one colon), so folding it costs nothing
// and keeps an address out of the response even if one were ever persisted.
func looksLikeAddress(key string) bool {
	if strings.Count(key, ":") >= 2 {
		return true
	}
	parts := strings.Split(key, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// cohortsOf turns a key→count map into a cohort slice.
func cohortsOf(counts, contributors map[string]int64) []cohort {
	out := make([]cohort, 0, len(counts))
	for k, v := range counts {
		n := contributors[k]
		if contributors == nil {
			n = v // compatibility for pure pre-projection fixtures
		}
		out = append(out, cohort{key: k, count: v, contributors: n})
	}
	return out
}

// buildReport assembles the response from persisted facts. It is pure: the same
// raw data, window, and clock always produce the same report.
//
// The cross-partition rule is what stops differencing. The daily series and the
// per-session dimensions (outcome, cli_version, os, arch, interactive,
// duration, projects, jobs) are all
// partitions of ONE quantity — sessions with a recorded outcome — so publishing
// that total, or any one partition's residual, lets a caller derive every other
// partition's residual by subtraction. The command mix is nominally a different
// quantity, but a session that invokes exactly one command makes the two totals
// equal, so it is a usable proxy for the same subtraction.
//
// Rather than model which quantity is a proxy for which, the contract takes the
// conservative rule: a total or a residual is published only when EVERY
// partition in the report has a safe residual. Individual cohorts at or above
// the threshold are always published — they leak nothing on their own — so the
// cost of the rule is that a single small cohort anywhere withholds the totals,
// not the distribution.
func buildReport(w Window, r raw, now time.Time) Report {
	dims := make(map[string]partition, len(reportDimensions))
	for _, dim := range reportDimensions {
		dims[dim] = suppress(cohortsOf(r.counters[dim], r.counterContributors[dim]), maxBucketsPerDimension)
	}
	daily := suppress(cohortsOf(r.dailySessions, r.dailySessionContributors), 0)

	residualsPublishable := daily.safe
	for _, dim := range reportDimensions {
		if !projectionUnavailable(r, dim) {
			residualsPublishable = residualsPublishable && dims[dim].safe
		}
	}
	if projectionUnavailable(r, DimOutcome) {
		residualsPublishable = false
	}

	devices := publishCount(r.devices)
	if projectionUnavailable(r, DimDevices) {
		devices = unavailableCount(ReasonProjectionOverflow)
	}
	sessions := coupledTotal(dims[DimOutcome].total, residualsPublishable &&
		dimensionContributorCount(r, DimOutcome) >= SuppressionThreshold)
	if projectionUnavailable(r, DimOutcome) {
		sessions = unavailableCount(ReasonProjectionOverflow)
	}
	commands := coupledTotal(dims[DimCommand].total, residualsPublishable &&
		dimensionContributorCount(r, DimCommand) >= SuppressionThreshold)
	if projectionUnavailable(r, DimCommand) {
		commands = unavailableCount(ReasonProjectionOverflow)
	}

	report := Report{
		Window:               w,
		SuppressionThreshold: SuppressionThreshold,
		Summary: Summary{
			Devices:  devices,
			Sessions: sessions,
			Commands: commands,
			Ingest: IngestHealth{
				AcceptedRecords: unavailableCount(ReasonNotPersisted),
				RejectedRecords: unavailableCount(ReasonNotPersisted),
			},
		},
		Daily:      dailySeries(w, r, daily),
		Breakdowns: breakdowns(dims, residualsPublishable, r.unavailable),
	}
	report.Freshness = freshness(report.Daily, now)
	return report
}

// coupledTotal publishes a total only when every partition in the report has a
// safe residual.
func coupledTotal(total int64, publishable bool) Count {
	if total == 0 {
		return available(0)
	}
	if !publishable {
		return suppressedCount()
	}
	return available(total)
}

// dailySeries emits one point per UTC day in the window. A day's session count
// is published when it is its own publishable cohort; a day with no sessions is
// published as zero, because an absence is not a cohort.
func dailySeries(w Window, r raw, daily partition) []DailyPoint {
	visible := make(map[string]int64, len(daily.visible))
	for _, b := range daily.visible {
		visible[b.Key] = b.Count
	}

	days := w.days()
	last := ""
	if len(days) > 0 {
		last = days[len(days)-1]
	}

	points := make([]DailyPoint, 0, len(days))
	for _, day := range days {
		point := DailyPoint{Day: day, Partial: day == last}
		point.Devices = publishCount(r.dailyDevices[day])
		if projectionUnavailable(r, DimDevices) {
			point.Devices = unavailableCount(ReasonProjectionOverflow)
		}
		if projectionUnavailable(r, DimOutcome) {
			point.Sessions = unavailableCount(ReasonProjectionOverflow)
			points = append(points, point)
			continue
		}
		switch v, ok := visible[day]; {
		case ok:
			point.Sessions = available(v)
		case r.dailySessions[day] == 0:
			point.Sessions = available(0)
		default:
			point.Sessions = suppressedCount()
		}
		points = append(points, point)
	}
	return points
}

func dimensionContributorCount(r raw, dim string) int64 {
	if r.dimensionContributors != nil {
		return r.dimensionContributors[dim]
	}
	var total int64
	for _, count := range r.counters[dim] {
		total += count
	}
	return total
}

func projectionUnavailable(r raw, dimension string) bool {
	return r.unavailable["*"] || r.unavailable[dimension]
}

// breakdowns emits the fixed dimension list in contract order.
func breakdowns(dims map[string]partition, residualsPublishable bool, unavailable map[string]bool) []Breakdown {
	out := make([]Breakdown, 0, len(reportDimensions))
	for _, dim := range reportDimensions {
		p := dims[dim]
		if unavailable["*"] || unavailable[dim] {
			out = append(out, Breakdown{
				Dimension: dim,
				State:     StateUnavailable,
				Reason:    ReasonProjectionOverflow,
				Buckets:   []Bucket{},
				Other:     unavailableCount(ReasonProjectionOverflow),
			})
			continue
		}
		buckets := p.visible
		if buckets == nil {
			buckets = []Bucket{}
		}
		other := suppressedCount()
		if residualsPublishable {
			other = available(p.residual)
		}
		out = append(out, Breakdown{Dimension: dim, State: StateAvailable, Buckets: buckets, Other: other})
	}
	return out
}

// freshness reports the most recent day whose own data is published, so it can
// never disclose a day the suppression rule withheld.
func freshness(points []DailyPoint, now time.Time) Freshness {
	f := Freshness{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		State:       StateUnavailable,
		Reason:      ReasonNoPublishedData,
	}
	for _, p := range points {
		if hasPublishedValue(p.Devices) || hasPublishedValue(p.Sessions) {
			f.State = StateAvailable
			f.Reason = ""
			f.LatestDay = p.Day
		}
	}
	return f
}

func hasPublishedValue(c Count) bool { return c.Available() && *c.Value > 0 }

// normalizeDay defensively pins a scanned day to the YYYY-MM-DD form.
func normalizeDay(day string) string {
	day = strings.TrimSpace(day)
	if len(day) > len(dayLayout) {
		day = day[:len(dayLayout)]
	}
	return day
}
