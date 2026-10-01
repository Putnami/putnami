package analytics

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ValidateBatch checks a decoded batch against the wire contract and returns one
// diagnostic per violation, in document order: the envelope first, then each
// event. An empty result means the batch conforms.
//
// It never stops at the first violation. A sanitizer that drops a whole batch
// still wants every reason, because the drop reasons are the metric.
func ValidateBatch(b *Batch) []diag.Diagnostic {
	if b == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "batch is nil")}
	}

	var diags []diag.Diagnostic
	if b.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidVersion, "protocolVersion",
			"protocolVersion is %d, the contract accepts %d", b.ProtocolVersion, ProtocolVersion))
	}
	if !TimestampRe.MatchString(b.SentAt) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTimestamp, "sentAt",
			"sentAt %q is not an RFC 3339 timestamp with milliseconds and Z", b.SentAt))
	}
	if len(b.Events) == 0 || len(b.Events) > MaxEvents {
		diags = append(diags, diag.Errorf(ErrorCodeBatchTooLarge, "events",
			"a batch carries 1..%d events, got %d", MaxEvents, len(b.Events)))
	}
	for i, event := range b.Events {
		diags = append(diags, ValidateEvent(event, fmt.Sprintf("events[%d]", i))...)
	}
	return diags
}

// ValidateEvent checks one event against the wire contract. field is the dotted
// path of the event inside the document it came from ("events[0]"), so a caller
// validating a single event out of band can name it whatever its own document
// calls it. An empty result means the event conforms.
func ValidateEvent(e Event, field string) []diag.Diagnostic {
	diags := validateEventIdentity(e, field)
	diags = append(diags, validateEventScalars(e, field)...)
	diags = append(diags, validateEventShape(e, field)...)
	if e.Page != nil {
		diags = append(diags, validatePage(*e.Page, field+".page")...)
	}
	return append(diags, validateProps(e.Props, field+".props")...)
}

// validateEventIdentity checks the four required scalars. A missing value is
// reported as missing_attribute before any format check, so an emitter that
// forgot a field is told that, not that the empty string is a bad UUID.
func validateEventIdentity(e Event, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	required := []struct {
		name  string
		value string
	}{
		{"eventId", e.EventID},
		{"name", e.Name},
		{"clientTs", e.ClientTs},
		{"sessionId", e.SessionID},
	}
	for _, r := range required {
		if r.value == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingAttribute, field+"."+r.name,
				"%s is required", r.name))
		}
	}

	if e.EventID != "" && !UUIDv7Re.MatchString(e.EventID) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidEventID, field+".eventId",
			"eventId %q is not a lowercase UUID v7", e.EventID))
	}
	if e.Name != "" && !IsEventName(e.Name) {
		diags = append(diags, diag.Errorf(ErrorCodeUnknownEvent, field+".name",
			"event name %q is not accepted on the wire (%s is recorded server-side only)",
			e.Name, EventFormSubmit))
	}
	if e.ClientTs != "" && !TimestampRe.MatchString(e.ClientTs) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTimestamp, field+".clientTs",
			"clientTs %q is not an RFC 3339 timestamp with milliseconds and Z", e.ClientTs))
	}
	if e.SessionID != "" && !UUIDv7Re.MatchString(e.SessionID) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidEventID, field+".sessionId",
			"sessionId %q is not a lowercase UUID v7", e.SessionID))
	}
	return diags
}

// validateEventScalars checks the bounded optional scalars. Seq is checked here
// too: JSON's absent integer and its zero decode identically, so 0 is a valid
// sequence number by construction and only the range is enforceable.
func validateEventScalars(e Event, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if e.Seq < 0 || e.Seq > MaxSeq {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+".seq",
			"seq is %d, the contract accepts 0..%d", e.Seq, MaxSeq))
	}
	if e.EngagementMs != nil && (*e.EngagementMs < 0 || *e.EngagementMs > MaxEngagementMs) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+".engagementMs",
			"engagementMs is %d, the contract accepts 0..%d", *e.EngagementMs, MaxEngagementMs))
	}
	if e.ViewportClass != "" && !IsViewportClass(e.ViewportClass) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+".viewportClass",
			"viewportClass %q is not one of %s", e.ViewportClass, strings.Join(ViewportClasses, ", ")))
	}
	if e.Language != "" && !LanguageRe.MatchString(e.Language) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, field+".language",
			"language %q is not a BCP 47 primary tag with an optional subtag", e.Language))
	}
	return diags
}

// validateEventShape checks which members an event kind may carry, then the
// format of the action name wherever one is present. The presence rules are
// keyed on the event name; the action-name format is not, so an action name
// smuggled onto a page view is reported as both misplaced and malformed.
func validateEventShape(e Event, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	switch e.Name {
	case EventPageView:
		if e.Page == nil {
			diags = append(diags, diag.Errorf(ErrorCodeMissingAttribute, field+".page",
				"%s requires page", EventPageView))
		}
		if e.Action != "" {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownAttribute, field+".action",
				"%s carries no action", EventPageView))
		}
		if e.Props != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownAttribute, field+".props",
				"%s carries no props", EventPageView))
		}
	case EventAction:
		if e.Page != nil {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownAttribute, field+".page",
				"%s carries no page", EventAction))
		}
		if e.Action == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingAttribute, field+".action",
				"%s requires action", EventAction))
		}
	}
	if e.Action != "" && !ActionNameRe.MatchString(e.Action) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidAction, field+".action",
			"action %q is not a lowercase name of at most %d characters", e.Action, MaxActionNameLen))
	}
	return diags
}

// validatePage checks the page member of a page view.
func validatePage(p Page, field string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	switch {
	case p.Path == "":
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPath, field+".path", "path is required"))
	case !strings.HasPrefix(p.Path, "/"):
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPath, field+".path",
			"path %q does not start with /", p.Path))
	case strings.ContainsAny(p.Path, "?#"):
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPath, field+".path",
			"path %q carries a query or a fragment; strip them before sending", p.Path))
	case len(p.Path) > MaxPathLen:
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPath, field+".path",
			"path is %d bytes, the contract accepts at most %d", len(p.Path), MaxPathLen))
	}

	if p.Route != "" && (len(p.Route) > MaxRouteLen || !RouteRe.MatchString(p.Route)) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRoute, field+".route",
			"route %q is not a route pattern of at most %d characters", p.Route, MaxRouteLen))
	}
	if p.Referrer != "" && !isReferrer(p.Referrer) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidReferrer, field+".referrer",
			"referrer %q is neither an absolute http(s) URL nor an app-relative path of at most %d characters",
			p.Referrer, MaxReferrerLen))
	}
	return append(diags, validateUTM(p.UTM, field+".utm")...)
}

// validateUTM checks the campaign parameters. Keys are walked in sorted order so
// two runs over the same map report the same diagnostics in the same order.
func validateUTM(utm map[string]string, field string) []diag.Diagnostic {
	diags := make([]diag.Diagnostic, 0, len(utm))
	for _, key := range sortedKeys(utm) {
		value := utm[key]
		at := field + "." + key
		switch {
		case !IsUTMKey(key):
			diags = append(diags, diag.Errorf(ErrorCodeInvalidUTM, at,
				"utm key %q is not one of %s", key, strings.Join(UTMKeys, ", ")))
		case value == "":
			diags = append(diags, diag.Errorf(ErrorCodeInvalidUTM, at, "utm %q is empty", key))
		case len(value) > MaxUTMLen:
			diags = append(diags, diag.Errorf(ErrorCodeInvalidUTM, at,
				"utm %q is %d bytes, the contract accepts at most %d", key, len(value), MaxUTMLen))
		}
	}
	if len(diags) == 0 {
		return nil
	}
	return diags
}

// validateProps checks the typed action properties. The count, the key grammar,
// and the value kinds are all bounded because every property becomes a daily
// counter key on the receiving side.
func validateProps(props map[string]any, field string) []diag.Diagnostic {
	if len(props) == 0 {
		return nil
	}
	var diags []diag.Diagnostic
	if len(props) > MaxPropKeys {
		diags = append(diags, diag.Errorf(ErrorCodePropsTooMany, field,
			"props carries %d keys, the contract accepts at most %d", len(props), MaxPropKeys))
	}
	for _, key := range sortedKeys(props) {
		at := field + "." + key
		if !PropKeyRe.MatchString(key) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPropKey, at,
				"prop key %q is not a lowercase name of at most %d characters", key, MaxPropKeyLen))
		}
		if reason, ok := propValueRejection(props[key]); !ok {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPropValue, at, "prop %q %s", key, reason))
		}
	}
	return diags
}

// propValueRejection reports whether a property value is one of the three
// accepted scalar kinds and, when it is not, why. A json.Number is accepted
// because a decoder configured with UseNumber produces one for the same input
// that yields a float64 by default.
func propValueRejection(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		if len(typed) > MaxPropStringLen {
			return fmt.Sprintf("is %d bytes, the contract accepts at most %d", len(typed), MaxPropStringLen), false
		}
		return "", true
	case bool:
		return "", true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return "is not a finite number", false
		}
		return "", true
	case json.Number:
		if _, err := typed.Float64(); err != nil {
			return "is not a finite number", false
		}
		return "", true
	default:
		return "is neither a bounded string, a finite number, nor a boolean", false
	}
}

// isReferrer reports whether a referrer is an absolute http(s) URL with a host
// or an app-relative path, and short enough to store.
//
// "//host/path" and "/\host/path" are NOT app-relative: browsers resolve both
// against the current scheme, so accepting them would file another origin under
// the internal referrer type and let a visitor forge internal traffic with a
// string that merely looks like a path.
func isReferrer(referrer string) bool {
	if len(referrer) > MaxReferrerLen {
		return false
	}
	if strings.HasPrefix(referrer, "/") {
		return len(referrer) < 2 || (referrer[1] != '/' && referrer[1] != '\\')
	}
	parsed, err := url.Parse(referrer)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// sortedKeys returns a map's keys in ascending order, so diagnostics derived
// from a map are deterministic.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// IsEventName reports whether name is accepted on the wire.
func IsEventName(name string) bool { return name == EventPageView || name == EventAction }

// IsViewportClass reports whether value is a known viewport class.
func IsViewportClass(value string) bool { return slices.Contains(ViewportClasses, value) }

// IsReferrerType reports whether value is a known referrer type.
func IsReferrerType(value string) bool { return slices.Contains(ReferrerTypes, value) }

// IsDeviceType reports whether value is a known device type.
func IsDeviceType(value string) bool { return slices.Contains(DeviceTypes, value) }

// IsBrowser reports whether value is a known browser family.
func IsBrowser(value string) bool { return slices.Contains(Browsers, value) }

// IsOS reports whether value is a known operating-system family.
func IsOS(value string) bool { return slices.Contains(OperatingSystems, value) }

// IsSource reports whether value is a known record source.
func IsSource(value string) bool { return slices.Contains(Sources, value) }

// IsVisitorKind reports whether value is a known visitor-id kind.
func IsVisitorKind(value string) bool { return slices.Contains(VisitorKinds, value) }

// IsOutcome reports whether value is a known form-submission outcome.
func IsOutcome(value string) bool { return slices.Contains(Outcomes, value) }

// IsDimension reports whether value is a known daily-counter dimension.
func IsDimension(value string) bool { return slices.Contains(Dimensions, value) }

// IsUTMKey reports whether value is an accepted campaign parameter.
func IsUTMKey(value string) bool { return slices.Contains(UTMKeys, value) }

// IsBotUserAgent reports whether a User-Agent is automated traffic, by
// case-insensitive substring match against the embedded token list. An empty
// User-Agent is a bot: a real browser always sends one, so treating the absence
// as human would make the cheapest possible forgery the one that counts.
func IsBotUserAgent(ua string) bool {
	if strings.TrimSpace(ua) == "" {
		return true
	}
	lowered := strings.ToLower(ua)
	for _, token := range botTokens() {
		if strings.Contains(lowered, token) {
			return true
		}
	}
	return false
}

// botTokens parses the embedded bot list once. A corrupt embed yields no tokens
// rather than a panic in a consumer's request path; the fixture test in this
// package is what pins the list's shape, size, and effect.
var botTokens = sync.OnceValue(func() []string {
	var document struct {
		Tokens []string `json:"tokens"`
	}
	if err := json.Unmarshal(BotsJSON, &document); err != nil {
		return nil
	}
	return document.Tokens
})
