// Package analytics owns the wire contract a Putnami web application's browser
// tracker uses to report page views and declared actions to its own server:
// POST /_putnami/analytics/events.
//
// The contract is deliberately closed. Every enum is a fixed list, every string
// is bounded, and action names and property keys must match a regular
// expression, because the receiving side folds these values into daily counters
// — an open vocabulary would be an unbounded metric series a visitor controls,
// which is a cardinality and a security problem at once. The Go code here is the
// contract, not a runtime: there is no Go web layer, so nothing in this package
// serves traffic. It exists so the TypeScript sanitizer in `@putnami/analytics`
// has an executable twin and a shared fixture corpus to prove parity against.
package analytics

import "regexp"

// ProtocolVersion is the only accepted value of Batch.ProtocolVersion.
const ProtocolVersion = 1

// Bounds of the wire contract. The TypeScript sanitizer mirrors every value.
const (
	// MaxEvents is the largest number of events one batch may carry.
	MaxEvents = 50
	// MaxBodyBytes is the largest accepted request body; a larger body is
	// answered with 413 and never parsed.
	MaxBodyBytes = 65536
	// MaxSeq is the highest per-session sequence number.
	MaxSeq = 1_000_000
	// MaxEngagementMs caps engagement time at 24 hours.
	MaxEngagementMs = 86_400_000
	// MaxPathLen bounds Page.Path.
	MaxPathLen = 512
	// MaxRouteLen bounds Page.Route.
	MaxRouteLen = 256
	// MaxReferrerLen bounds Page.Referrer.
	MaxReferrerLen = 512
	// MaxUTMLen bounds each Page.UTM value.
	MaxUTMLen = 128
	// MaxActionNameLen bounds Event.Action; ActionNameRe enforces it too.
	MaxActionNameLen = 64
	// MaxPropKeys is the largest number of keys Event.Props may carry.
	MaxPropKeys = 20
	// MaxPropKeyLen bounds each Event.Props key; PropKeyRe enforces it too.
	MaxPropKeyLen = 32
	// MaxPropStringLen bounds a string-valued entry of Event.Props.
	MaxPropStringLen = 256
)

// Event names accepted on the wire. EventFormSubmit is server-only and is
// listed so consumers share the vocabulary; a wire batch carrying it is rejected.
const (
	// EventPageView reports one viewed page.
	EventPageView = "page_view"
	// EventAction reports one declared, server-known action.
	EventAction = "action"
	// EventFormSubmit is recorded by the server, never accepted from the wire.
	EventFormSubmit = "form_submit"
)

// Closed enums. Each has an Is<Enum>(string) bool predicate in validate.go.
var (
	// ViewportClasses are the client viewport buckets.
	ViewportClasses = []string{"xs", "sm", "md", "lg", "xl"}
	// ReferrerTypes classify where a visit came from.
	ReferrerTypes = []string{"direct", "internal", "search", "social", "other"}
	// DeviceTypes classify the device a visit came from.
	DeviceTypes = []string{"desktop", "mobile", "tablet", "other"}
	// Browsers is the closed browser-family vocabulary.
	Browsers = []string{"chrome", "safari", "firefox", "edge", "opera", "samsung", "other"}
	// OperatingSystems is the closed operating-system-family vocabulary.
	OperatingSystems = []string{"windows", "macos", "ios", "android", "linux", "chromeos", "other"}
	// Sources say whether the server or the browser emitted a record.
	Sources = []string{"server", "client"}
	// VisitorKinds say how a visitor id was derived.
	VisitorKinds = []string{"daily", "cookie"}
	// Outcomes classify a recorded form submission.
	Outcomes = []string{"ok", "validation_error", "error"}
	// UTMKeys is the closed set of accepted campaign parameters.
	UTMKeys = []string{"source", "medium", "campaign", "content", "term"}
	// Dimensions is the closed set of daily counter dimensions (body §B.2).
	Dimensions = []string{"event", "route", "path", "referrer_host", "referrer_type", "utm_source", "utm_medium",
		"utm_campaign", "country", "device_type", "browser", "os", "language", "action", "form_submit"}
)

// Regular expressions of the wire contract (body §A.1–A.2), compiled once.
var (
	// TimestampRe accepts RFC 3339 with milliseconds and a literal Z.
	TimestampRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	// UUIDv7Re accepts a lowercase UUID version 7 with an RFC 4122 variant.
	UUIDv7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	// RouteRe accepts a Putnami route pattern: literal segments plus [param]
	// and [...rest] placeholders, or the root route.
	RouteRe = regexp.MustCompile(`^/([A-Za-z0-9._~\-]+|\[[A-Za-z0-9_]+\]|\[\.\.\.[A-Za-z0-9_]+\])(/([A-Za-z0-9._~\-]+|\[[A-Za-z0-9_]+\]|\[\.\.\.[A-Za-z0-9_]+\]))*$|^/$`)
	// ActionNameRe accepts a declared action name.
	ActionNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// PropKeyRe accepts an action property key.
	PropKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// LanguageRe accepts the primary BCP 47 tag with an optional subtag.
	LanguageRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)
)

// Error codes of the analytics wire contract. They are the closed drop-reason
// vocabulary the TypeScript sanitizer counts on (minus the `analytics.` prefix),
// so renaming one silently stops a deployed consumer from counting.
const (
	// ErrorCodeParseError reports a body that is not a decodable batch.
	ErrorCodeParseError = "analytics.parse_error"
	// ErrorCodeInvalidVersion reports a protocolVersion other than 1.
	ErrorCodeInvalidVersion = "analytics.invalid_version"
	// ErrorCodeBatchTooLarge reports an empty or over-long events array.
	ErrorCodeBatchTooLarge = "analytics.batch_too_large"
	// ErrorCodeInvalidTimestamp reports a timestamp outside TimestampRe.
	ErrorCodeInvalidTimestamp = "analytics.invalid_timestamp"
	// ErrorCodeInvalidEventID reports an id outside UUIDv7Re.
	ErrorCodeInvalidEventID = "analytics.invalid_event_id"
	// ErrorCodeUnknownEvent reports an event name outside the wire vocabulary.
	ErrorCodeUnknownEvent = "analytics.unknown_event"
	// ErrorCodeUnknownAttribute reports a field the contract does not define,
	// or a field present on the wrong event kind.
	ErrorCodeUnknownAttribute = "analytics.unknown_attribute"
	// ErrorCodeMissingAttribute reports a required field that is absent.
	ErrorCodeMissingAttribute = "analytics.missing_attribute"
	// ErrorCodeAttributeKind reports a field carrying the wrong JSON type.
	ErrorCodeAttributeKind = "analytics.attribute_kind"
	// ErrorCodeInvalidValue reports a scalar outside its declared bounds.
	ErrorCodeInvalidValue = "analytics.invalid_value"
	// ErrorCodeInvalidPath reports a page path outside its contract.
	ErrorCodeInvalidPath = "analytics.invalid_path"
	// ErrorCodeInvalidRoute reports a route pattern outside RouteRe.
	ErrorCodeInvalidRoute = "analytics.invalid_route"
	// ErrorCodeInvalidReferrer reports a referrer that is neither an absolute
	// http(s) URL nor an app-relative path.
	ErrorCodeInvalidReferrer = "analytics.invalid_referrer"
	// ErrorCodeInvalidUTM reports an unknown or out-of-bounds campaign value.
	ErrorCodeInvalidUTM = "analytics.invalid_utm"
	// ErrorCodePropsTooMany reports more than MaxPropKeys action properties.
	ErrorCodePropsTooMany = "analytics.props_too_many"
	// ErrorCodeInvalidPropKey reports a property key outside PropKeyRe.
	ErrorCodeInvalidPropKey = "analytics.invalid_prop_key"
	// ErrorCodeInvalidPropValue reports a property value outside the accepted
	// scalar kinds.
	ErrorCodeInvalidPropValue = "analytics.invalid_prop_value"
	// ErrorCodeInvalidAction reports an action name outside ActionNameRe.
	ErrorCodeInvalidAction = "analytics.invalid_action"
)

// ValidErrorCodes is the closed set of codes this package may emit. A consumer
// branches on these strings, so the set is pinned by conformance tests and the
// fixture corpus carries one invalid batch per code (parse_error excepted: a
// corpus of valid JSON cannot express a body that is not JSON).
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:       true,
	ErrorCodeInvalidVersion:   true,
	ErrorCodeBatchTooLarge:    true,
	ErrorCodeInvalidTimestamp: true,
	ErrorCodeInvalidEventID:   true,
	ErrorCodeUnknownEvent:     true,
	ErrorCodeUnknownAttribute: true,
	ErrorCodeMissingAttribute: true,
	ErrorCodeAttributeKind:    true,
	ErrorCodeInvalidValue:     true,
	ErrorCodeInvalidPath:      true,
	ErrorCodeInvalidRoute:     true,
	ErrorCodeInvalidReferrer:  true,
	ErrorCodeInvalidUTM:       true,
	ErrorCodePropsTooMany:     true,
	ErrorCodeInvalidPropKey:   true,
	ErrorCodeInvalidPropValue: true,
	ErrorCodeInvalidAction:    true,
}

// Batch is the request body of POST /_putnami/analytics/events.
type Batch struct {
	// ProtocolVersion must equal ProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// SentAt is the client clock at send time, RFC 3339 with milliseconds and Z.
	SentAt string `json:"sentAt"`
	// Events carries 1..MaxEvents events.
	Events []Event `json:"events"`
}

// Event is one page view or one declared action.
type Event struct {
	// EventID is the emitter-generated UUID v7 that dedups the event.
	EventID string `json:"eventId"`
	// Name is EventPageView or EventAction.
	Name string `json:"name"`
	// ClientTs is the client clock when the event happened.
	ClientTs string `json:"clientTs"`
	// Seq is the per-session sequence number.
	Seq int `json:"seq"`
	// SessionID is the client-owned session UUID v7.
	SessionID string `json:"sessionId"`
	// EngagementMs is visible time on the page, when known.
	EngagementMs *int `json:"engagementMs,omitempty"`
	// ViewportClass is one of ViewportClasses, when known.
	ViewportClass string `json:"viewportClass,omitempty"`
	// Language is the primary BCP 47 tag, when known.
	Language string `json:"language,omitempty"`
	// Page is required for page_view and forbidden otherwise.
	Page *Page `json:"page,omitempty"`
	// Action is the declared action name; required for action and forbidden otherwise.
	Action string `json:"action,omitempty"`
	// Props are the typed action properties (≤ MaxPropKeys).
	Props map[string]any `json:"props,omitempty"`
}

// Page describes the viewed page.
type Page struct {
	// Path is the app-relative path without query or fragment.
	Path string `json:"path"`
	// Route is the matched route pattern, when the client knows it.
	Route string `json:"route,omitempty"`
	// Referrer is an absolute http(s) URL or an app-relative path.
	Referrer string `json:"referrer,omitempty"`
	// UTM carries the landing-page campaign parameters.
	UTM map[string]string `json:"utm,omitempty"`
}
