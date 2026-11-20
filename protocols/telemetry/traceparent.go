package telemetry

import "strings"

// Proxy-safe trace context. Some managed proxies insert a span of their own
// and rewrite the parent ID of traceparent without exporting that span, so the
// server span would point at a parent missing from the trace. A client sends
// its span context twice: in traceparent, and in HeaderPutnamiTraceparent,
// which proxies leave alone. A server picks its remote parent by
// ResolveRemoteParent.
//
// The header name and the rule are also committed in
// fixtures/conventions/proxy-safe-traceparent.json, which the TypeScript
// runtime reads in its tests; traceparent_test.go keeps this file equal to
// that fixture.
const (
	// HeaderTraceparent is the W3C Trace Context header.
	HeaderTraceparent = "traceparent"
	// HeaderPutnamiTraceparent carries the same value as traceparent, written
	// by the client that made the call. Proxies do not rewrite it.
	HeaderPutnamiTraceparent = "x-putnami-traceparent"

	// RemoteParentNone names a request with no usable remote parent.
	RemoteParentNone = "none"
	// RemoteParentTraceparent names a parent read from traceparent.
	RemoteParentTraceparent = "traceparent"
	// RemoteParentPutnami names a parent read from HeaderPutnamiTraceparent.
	RemoteParentPutnami = "x-putnami-traceparent"
)

// Traceparent is a parsed version 00 traceparent value.
type Traceparent struct {
	// TraceID is 32 lowercase hex characters, not all zero.
	TraceID string `json:"traceId"`
	// SpanID is 16 lowercase hex characters, not all zero.
	SpanID string `json:"spanId"`
	// TraceFlags is 2 lowercase hex characters.
	TraceFlags string `json:"traceFlags"`
}

// String renders the value as a version 00 traceparent.
func (t Traceparent) String() string {
	return "00-" + t.TraceID + "-" + t.SpanID + "-" + t.TraceFlags
}

// ParseTraceparent reads a version 00 traceparent: 00, a 32-character trace
// ID, a 16-character span ID and 2 flag characters, all lowercase hex and
// separated by dashes. An all-zero trace or span ID is invalid.
func ParseTraceparent(value string) (Traceparent, bool) {
	if len(value) != 55 || value[0:3] != "00-" || value[35] != '-' || value[52] != '-' {
		return Traceparent{}, false
	}
	parsed := Traceparent{TraceID: value[3:35], SpanID: value[36:52], TraceFlags: value[53:55]}
	if !lowerHex(parsed.TraceID) || !lowerHex(parsed.SpanID) || !lowerHex(parsed.TraceFlags) {
		return Traceparent{}, false
	}
	if strings.Trim(parsed.TraceID, "0") == "" || strings.Trim(parsed.SpanID, "0") == "" {
		return Traceparent{}, false
	}
	return parsed, true
}

// ResolveRemoteParent picks the remote parent of a server span from the
// traceparent and HeaderPutnamiTraceparent values of a request, and names the
// header it came from.
//
// Without a valid traceparent there is no remote parent. When the second
// header is valid, carries the same trace ID and a different parent ID, a
// proxy rewrote traceparent: the parent is the second header's. Otherwise,
// with the second header absent, invalid or on another trace, the parent is
// traceparent's.
func ResolveRemoteParent(traceparent, putnamiTraceparent string) (Traceparent, string) {
	primary, ok := ParseTraceparent(traceparent)
	if !ok {
		return Traceparent{}, RemoteParentNone
	}
	second, ok := ParseTraceparent(putnamiTraceparent)
	if ok && second.TraceID == primary.TraceID && second.SpanID != primary.SpanID {
		return second, RemoteParentPutnami
	}
	return primary, RemoteParentTraceparent
}

// RemoteParentOutside reports whether the remote parent of a request came
// from outside: traceparent is valid and putnamiTraceparent is not a valid
// value on the same trace. Every Putnami client sends both headers, so a
// parent it sent is never outside; a load balancer's edge or a browser sends
// traceparent only. A request with no valid traceparent has no remote parent
// and reports false. Under ParentDecide, a server span whose parent is outside
// takes a root decision.
func RemoteParentOutside(traceparent, putnamiTraceparent string) bool {
	primary, ok := ParseTraceparent(traceparent)
	if !ok {
		return false
	}
	second, ok := ParseTraceparent(putnamiTraceparent)
	return !ok || second.TraceID != primary.TraceID
}

func lowerHex(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
