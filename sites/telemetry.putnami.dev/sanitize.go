package main

import (
	"strconv"

	diag "go.putnami.dev/protocol/diagnostic"
	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"
)

// Provenance markers. origin is stamped at the resource level after every clean
// pass so it is unspoofable: a caller-supplied origin (at resource OR record
// level) is always dropped before this is written.
const (
	attrOrigin    = "origin"
	originCLIAnon = "cli-anon"
)

// sanitizeLogs rebuilds an inbound OTLP logs request into a clean, vocabulary-
// conformant request. Between parse and persist it: drops any resource that is
// not CLI-usage telemetry (service.name must be putnami-cli); stamps
// origin=cli-anon at the resource level (caller value dropped); strips unknown
// attribute keys; enforces the S2a value kinds and closed enums through
// cliusage; derives body, severity, scope, and resource identity from fixed
// values; and admits record strings only through strict value contracts. Any
// record that cannot be normalized is dropped (fail-silent). The returned
// request contains only conforming resource logs and may be empty.
func sanitizeLogs(req *telemetry.LogsRequest) telemetry.LogsRequest {
	var out telemetry.LogsRequest
	for _, rl := range req.ResourceLogs {
		if !resourceIsCLIUsage(rl.Resource) {
			continue // not CLI usage telemetry — drop wholesale, never relabel
		}
		var scopes []telemetry.ScopeLogs
		for _, sl := range rl.ScopeLogs {
			var records []telemetry.LogRecord
			for _, rec := range sl.LogRecords {
				if clean, ok := sanitizeRecord(rec); ok {
					records = append(records, clean)
				}
			}
			if len(records) == 0 {
				continue
			}
			scopes = append(scopes, telemetry.ScopeLogs{
				Scope:      sanitizeScope(sl.Scope),
				LogRecords: records,
			})
		}
		if len(scopes) == 0 {
			continue
		}
		out.ResourceLogs = append(out.ResourceLogs, telemetry.ResourceLogs{
			Resource:  sanitizeResource(rl.Resource),
			ScopeLogs: scopes,
		})
	}
	return out
}

// resourceIsCLIUsage reports whether the resource declares
// service.name=putnami-cli. The receiver only accepts CLI
// usage telemetry; anything else is dropped rather than relabeled.
func resourceIsCLIUsage(res telemetry.Resource) bool {
	for _, kv := range res.Attributes {
		if kv.Key != telemetry.AttrServiceName {
			continue
		}
		return kv.Value.StringValue != nil && valueKindCount(kv.Value) == 1 &&
			*kv.Value.StringValue == cliusage.ServiceName
	}
	return false
}

// sanitizeResource emits only receiver-owned constants plus a strictly
// validated CLI version. Caller-supplied resource strings are never preserved.
func sanitizeResource(res telemetry.Resource) telemetry.Resource {
	attrs := map[string]string{
		attrOrigin:                     originCLIAnon,
		telemetry.AttrServiceName:      cliusage.ServiceName,
		telemetry.AttrPutnamiFramework: cliusage.Framework,
	}
	for _, kv := range res.Attributes {
		if kv.Key == telemetry.AttrServiceVersion && kv.Value.StringValue != nil &&
			valueKindCount(kv.Value) == 1 && cliusage.IsCLIVersion(*kv.Value.StringValue) {
			attrs[telemetry.AttrServiceVersion] = *kv.Value.StringValue
		}
	}
	return telemetry.Resource{Attributes: telemetry.AttrsFromStrings(attrs)}
}

// sanitizeScope pins the scope name and retains only a valid CLI version.
func sanitizeScope(scope telemetry.Scope) telemetry.Scope {
	clean := telemetry.Scope{Name: cliusage.ScopeName}
	if cliusage.IsCLIVersion(scope.Version) {
		clean.Version = scope.Version
	}
	return clean
}

// sanitizeRecord rebuilds one log record clean. It returns ok=false (drop the
// record) when the record is not a known CLI usage event, or when the rebuilt
// record fails the shared cliusage vocabulary (missing required attribute, bad
// value kind, unknown error category, success/category inconsistency). Trace
// correlation IDs are dropped: they carry no value for anonymous usage
// telemetry and would be an unnecessary free-form surface.
func sanitizeRecord(rec telemetry.LogRecord) (telemetry.LogRecord, bool) {
	eventName, ok := recordEventName(rec)
	if !ok {
		return telemetry.LogRecord{}, false
	}
	allowed, known := cliusage.AllowedAttributes[eventName]
	if !known {
		return telemetry.LogRecord{}, false
	}

	attrs := make([]telemetry.KeyValue, 0, len(rec.Attributes))
	seen := make(map[string]struct{}, len(rec.Attributes))
	for _, kv := range rec.Attributes {
		if _, dup := seen[kv.Key]; dup {
			continue // drop duplicate keys — keep the first valid occurrence
		}
		kind, allowedKey := expectedKind(kv.Key, allowed)
		if !allowedKey {
			continue // strip unknown key
		}
		clean, ok := rebuildValue(kv.Key, kv.Value, kind)
		if !ok {
			continue // wrong/multiple kind or invalid closed value → drop attr
		}
		seen[kv.Key] = struct{}{}
		attrs = append(attrs, telemetry.KeyValue{Key: kv.Key, Value: clean})
	}
	telemetry.SortAttrs(attrs)

	clean := telemetry.LogRecord{
		TimeUnixNano:         cleanNumericString(rec.TimeUnixNano),
		ObservedTimeUnixNano: cleanNumericString(rec.ObservedTimeUnixNano),
		SeverityNumber:       telemetry.SeverityInfo,
		SeverityText:         "info",
		Attributes:           attrs,
	}
	body := telemetry.StringVal(eventName)
	clean.Body = &body

	// Enforce the S2a vocabulary through cliusage so producer and receiver
	// cannot drift. A record that does not conform is dropped (fail-silent).
	if diag.HasErrors(cliusage.ValidateRecord(clean)) {
		return telemetry.LogRecord{}, false
	}
	return clean, true
}

// recordEventName extracts the event.name string attribute, if present.
func recordEventName(rec telemetry.LogRecord) (string, bool) {
	for _, kv := range rec.Attributes {
		if kv.Key == cliusage.AttrEventName {
			if kv.Value.StringValue == nil || valueKindCount(kv.Value) != 1 {
				return "", false
			}
			name := *kv.Value.StringValue
			return name, cliusage.IsEvent(name)
		}
	}
	return "", false
}

// expectedKind returns the wire value kind an attribute key must carry: envelope
// keys are valid on every record; business keys are valid only for their event.
func expectedKind(key string, allowed map[string]cliusage.ValueKind) (cliusage.ValueKind, bool) {
	if kind, ok := cliusage.EnvelopeAttributes[key]; ok {
		return kind, true
	}
	if kind, ok := allowed[key]; ok {
		return kind, true
	}
	return 0, false
}

// rebuildValue reconstructs a scalar AnyValue from its typed pointer, enforcing
// exactly one value kind, the expected kind, the string backstop, and canonical
// integer formatting. It never reuses the caller's AnyValue.
func rebuildValue(key string, v telemetry.AnyValue, kind cliusage.ValueKind) (telemetry.AnyValue, bool) {
	if valueKindCount(v) != 1 {
		return telemetry.AnyValue{}, false
	}
	switch kind {
	case cliusage.KindString:
		if v.StringValue == nil {
			return telemetry.AnyValue{}, false
		}
		s := *v.StringValue
		if !cliusage.IsStringAttributeValue(key, s) {
			return telemetry.AnyValue{}, false
		}
		return telemetry.StringVal(s), true
	case cliusage.KindInt:
		if v.IntValue == nil {
			return telemetry.AnyValue{}, false
		}
		n, err := strconv.ParseInt(*v.IntValue, 10, 64)
		if err != nil {
			return telemetry.AnyValue{}, false
		}
		return telemetry.IntVal(n), true
	case cliusage.KindBool:
		if v.BoolValue == nil {
			return telemetry.AnyValue{}, false
		}
		return telemetry.BoolVal(*v.BoolValue), true
	default:
		return telemetry.AnyValue{}, false
	}
}

// valueKindCount counts how many scalar kinds an AnyValue sets. Exactly one is
// required for a well-formed attribute.
func valueKindCount(v telemetry.AnyValue) int {
	n := 0
	if v.StringValue != nil {
		n++
	}
	if v.BoolValue != nil {
		n++
	}
	if v.IntValue != nil {
		n++
	}
	if v.DoubleValue != nil {
		n++
	}
	return n
}

// cleanNumericString keeps a numeric OTLP string field (unix-nano timestamps)
// only when it is all decimal digits; anything else is dropped to keep the
// emitted envelope structurally valid.
func cleanNumericString(s string) string {
	if s == "" {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return ""
		}
	}
	return s
}
