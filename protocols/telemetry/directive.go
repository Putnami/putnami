package telemetry

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Export directive conventions. An OTLP endpoint may answer an export with a
// directive header that sets the trace settings of the service that sent it
// and tightens its log level, for a bounded time, and may slow a signal down
// with a 429 and a Retry-After. Putnami Cloud renders the settings a person
// stored, so a directive's trace settings win over the service's own, higher
// or lower (ADR 0002). Every runtime parses the header and applies the
// directive by the same rule, so a person tunes Go and TypeScript services
// alike.
//
// fixtures/conventions/export-directives.json holds the same values and cases;
// directive_test.go keeps them equal.
const (
	// HeaderExportDirective is the response header that carries a directive.
	HeaderExportDirective = "Putnami-Export-Directive"

	// DirectiveTraceBudget sets telemetry.traces.budget: a whole number of 0
	// or more.
	DirectiveTraceBudget = "trace-budget"
	// DirectiveTraceRatio sets telemetry.traces.ratio: a number in [0, 1].
	DirectiveTraceRatio = "trace-ratio"
	// DirectiveTraceFloor sets telemetry.traces.floorPerMinute: a whole
	// number of 0 or more.
	DirectiveTraceFloor = "trace-floor"
	// DirectiveTraceParent sets telemetry.traces.parent: ParentFollow or
	// ParentDecide.
	DirectiveTraceParent = "trace-parent"
	// DirectiveLogLevel raises the minimum log level: debug, info, warn or
	// error.
	DirectiveLogLevel = "log-level"
	// DirectiveTTL is how many seconds the directive holds: a whole number of
	// 1 or more. It is required.
	DirectiveTTL = "ttl"

	// MaxDirectiveTTLSeconds caps a directive's ttl, so an endpoint that goes
	// away never pins a service to its last directive for long.
	MaxDirectiveTTLSeconds = 3600
	// MaxRetryAfterSeconds caps the delay a 429 Retry-After imposes on a
	// signal.
	MaxRetryAfterSeconds = 3600
)

// ExportDirective is one parsed directive. A nil or empty setting leaves the
// service's own; LogLevel is empty when the directive sets no level.
type ExportDirective struct {
	// TraceBudget sets the trace budget.
	TraceBudget *int64
	// TraceRatio sets the trace ratio.
	TraceRatio *float64
	// TraceFloorPerMinute sets the trace floor.
	TraceFloorPerMinute *int64
	// TraceParent sets the parent policy: ParentFollow or ParentDecide.
	TraceParent string
	// LogLevel raises the minimum log level: debug, info, warn or error.
	LogLevel string
	// TTL is how long the directive holds from its arrival.
	TTL time.Duration
}

// ParseExportDirective parses the value of HeaderExportDirective. It reports
// false when the header is void: a known key with a malformed value, a
// repeated key, a member that is not key=value, or no valid ttl. An unknown
// key is ignored, so an endpoint may add keys without breaking older
// services. A ttl above MaxDirectiveTTLSeconds is capped.
func ParseExportDirective(value string) (ExportDirective, bool) {
	var directive ExportDirective
	seen := map[string]bool{}
	for _, member := range strings.Split(value, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		key, raw, ok := strings.Cut(member, "=")
		if !ok {
			return ExportDirective{}, false
		}
		key, raw = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(raw)
		if seen[key] {
			return ExportDirective{}, false
		}
		seen[key] = true
		switch key {
		case DirectiveTraceBudget:
			budget, ok := wholeNumber(raw)
			if !ok {
				return ExportDirective{}, false
			}
			directive.TraceBudget = &budget
		case DirectiveTraceParent:
			parent := strings.ToLower(raw)
			if parent != ParentFollow && parent != ParentDecide {
				return ExportDirective{}, false
			}
			directive.TraceParent = parent
		case DirectiveTraceRatio:
			ratio, err := strconv.ParseFloat(raw, 64)
			if err != nil || !decimal(raw) || math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
				return ExportDirective{}, false
			}
			directive.TraceRatio = &ratio
		case DirectiveTraceFloor:
			floor, ok := wholeNumber(raw)
			if !ok {
				return ExportDirective{}, false
			}
			directive.TraceFloorPerMinute = &floor
		case DirectiveLogLevel:
			level := strings.ToLower(raw)
			if logLevelRank(level) < 0 {
				return ExportDirective{}, false
			}
			directive.LogLevel = level
		case DirectiveTTL:
			seconds, ok := wholeNumber(raw)
			if !ok || seconds < 1 {
				return ExportDirective{}, false
			}
			directive.TTL = time.Duration(min(seconds, MaxDirectiveTTLSeconds)) * time.Second
		}
	}
	if directive.TTL == 0 {
		return ExportDirective{}, false
	}
	return directive, true
}

// ApplyTraceSampling returns own with each trace setting the directive
// carries in its place, higher or lower: the budget, the ratio, the floor and
// the parent policy. A setting the directive leaves out keeps own's.
func (d ExportDirective) ApplyTraceSampling(own TraceSampling) TraceSampling {
	if d.TraceBudget != nil {
		own.Budget = *d.TraceBudget
	}
	if d.TraceRatio != nil {
		own.Ratio = *d.TraceRatio
	}
	if d.TraceFloorPerMinute != nil {
		own.FloorPerMinute = *d.TraceFloorPerMinute
	}
	if d.TraceParent != "" {
		own.Parent = d.TraceParent
	}
	return own
}

// LimitLogLevel returns the stricter of own, a level of LogLevels, and the
// directive's level: the higher one. Error is the highest level a directive
// sets, so a record at error is always emitted.
func (d ExportDirective) LimitLogLevel(own string) string {
	if logLevelRank(d.LogLevel) > logLevelRank(own) {
		return d.LogLevel
	}
	return own
}

func logLevelRank(level string) int {
	for rank, known := range LogLevels() {
		if level == known {
			return rank
		}
	}
	return -1
}

// wholeNumber parses a whole number of 0 or more written in ASCII digits. A
// number too large for an int64 reads as math.MaxInt64, so every runtime caps
// it alike.
func wholeNumber(raw string) (int64, bool) {
	if raw == "" || strings.TrimLeft(raw, "0123456789") != "" {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return math.MaxInt64, true
	}
	return value, true
}

// decimal reports whether raw is digits with at most one decimal point, so
// forms such as 1e-2, +0.5 or Inf are refused in every runtime alike.
func decimal(raw string) bool {
	whole, fraction, _ := strings.Cut(raw, ".")
	digits := func(s string) bool { return strings.TrimLeft(s, "0123456789") == "" }
	return (whole != "" || fraction != "") && digits(whole) && digits(fraction)
}

// RetryAfterDelay returns how long a signal waits before its next export after
// a response with status and the Retry-After value: delta-seconds or an
// IMF-fixdate HTTP-date, for a 429 only, at most MaxRetryAfterSeconds. Any other status,
// or a value of neither form, gives zero.
func RetryAfterDelay(status int, value string, now time.Time) time.Duration {
	if status != http.StatusTooManyRequests {
		return 0
	}
	value = strings.TrimSpace(value)
	maxDelay := time.Duration(MaxRetryAfterSeconds) * time.Second
	if seconds, ok := wholeNumber(value); ok {
		if seconds > MaxRetryAfterSeconds {
			return maxDelay
		}
		return time.Duration(seconds) * time.Second
	}
	// Only the exact IMF-fixdate form a formatter writes back is a date, so
	// case, one-digit fields, an impossible day or a wrong weekday read as
	// no delay in every runtime.
	at, err := time.Parse(http.TimeFormat, value)
	if err != nil || at.Format(http.TimeFormat) != value {
		return 0
	}
	delay := at.Sub(now)
	if delay < 0 {
		return 0
	}
	return min(delay, maxDelay)
}
