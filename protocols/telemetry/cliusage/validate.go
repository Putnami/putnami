package cliusage

import (
	"fmt"

	diag "go.putnami.dev/protocol/diagnostic"
	telemetry "go.putnami.dev/protocol/telemetry"
)

// Error codes for CLI usage vocabulary violations. The receiver's sanitizer
// keys its abuse metrics off these codes, so they are stable automation
// vocabulary: rename one and a deployed consumer stops counting.
const (
	ErrorCodeUnknownEvent         = "cliusage.unknown_event"
	ErrorCodeUnknownAttribute     = "cliusage.unknown_attribute"
	ErrorCodeAttributeKind        = "cliusage.attribute_kind"
	ErrorCodeMissingAttribute     = "cliusage.missing_attribute"
	ErrorCodeUnknownErrorCategory = "cliusage.unknown_error_category"
	ErrorCodeSuccessConsistency   = "cliusage.success_category_consistency"
	ErrorCodeInvalidService       = "cliusage.invalid_service"
	ErrorCodeInvalidValue         = "cliusage.invalid_value"
)

// ValidateLogs validates an entire OTLP logs request as CLI usage telemetry: the
// resource must identify service.name=putnami-cli, and every record must conform
// to the vocabulary. Structural OTLP validity (severity range, exactly one value
// kind per attribute, etc.) is the caller's responsibility — pair this with
// telemetry.ParseAndValidateLogs, which the receiver runs first. Returns empty
// diagnostics when the request conforms.
func ValidateLogs(req telemetry.LogsRequest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for ri, rl := range req.ResourceLogs {
		rf := fmt.Sprintf("resourceLogs[%d]", ri)
		diags = append(diags, validateResourceService(rl.Resource, rf+".resource")...)
		for si, sl := range rl.ScopeLogs {
			sf := fmt.Sprintf("%s.scopeLogs[%d]", rf, si)
			for li, record := range sl.LogRecords {
				diags = append(diags, validateRecord(record, fmt.Sprintf("%s.logRecords[%d]", sf, li))...)
			}
		}
	}
	return diags
}

// ValidateRecord checks one decoded OTLP log record against the CLI usage
// vocabulary: a known event, only allowed attribute keys, the right value kind
// per key, a valid error category, the required attributes present, and
// success/errorCategory consistency. Returns empty diagnostics when the record
// conforms.
func ValidateRecord(record telemetry.LogRecord) []diag.Diagnostic {
	return validateRecord(record, "logRecord")
}

func validateRecord(record telemetry.LogRecord, field string) []diag.Diagnostic {
	attrs := make(map[string]telemetry.AnyValue, len(record.Attributes))
	for _, kv := range record.Attributes {
		attrs[kv.Key] = kv.Value
	}

	eventName, ok := stringValue(attrs[AttrEventName])
	if !ok {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownEvent, field+".attributes."+AttrEventName,
			"log record is missing the %s attribute", AttrEventName)}
	}
	allowed, known := AllowedAttributes[eventName]
	if !known {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownEvent, field+".attributes."+AttrEventName,
			"event %q is not in the CLI usage vocabulary", eventName)}
	}

	var diags []diag.Diagnostic
	for _, kv := range record.Attributes {
		at := field + ".attributes." + kv.Key
		kind, isEnvelope := EnvelopeAttributes[kv.Key]
		if !isEnvelope {
			var isBusiness bool
			if kind, isBusiness = allowed[kv.Key]; !isBusiness {
				diags = append(diags, diag.Errorf(ErrorCodeUnknownAttribute, at,
					"attribute %q is not allowed on %s", kv.Key, eventName))
				continue
			}
		}
		if got, ok := valueKindOf(kv.Value); !ok || got != kind {
			diags = append(diags, diag.Errorf(ErrorCodeAttributeKind, at,
				"attribute %q must carry %s", kv.Key, kindName(kind)))
			continue
		}
		if kv.Key == AttrErrorCategory && !IsErrorCategory(*kv.Value.StringValue) {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownErrorCategory, at,
				"error category %q is not in the vocabulary", *kv.Value.StringValue))
		}
		if kv.Value.StringValue != nil && !IsStringAttributeValue(kv.Key, *kv.Value.StringValue) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidValue, at,
				"attribute %q is outside its closed value contract", kv.Key))
		}
	}

	for key := range RequiredAttributes[eventName] {
		if _, present := attrs[key]; !present {
			diags = append(diags, diag.Errorf(ErrorCodeMissingAttribute, field+".attributes."+key,
				"%s is missing required attribute %q", eventName, key))
		}
	}
	for key := range EnvelopeAttributes {
		if _, present := attrs[key]; !present {
			diags = append(diags, diag.Errorf(ErrorCodeMissingAttribute, field+".attributes."+key,
				"log record is missing required envelope attribute %q", key))
		}
	}

	if eventName == EventSessionEnd {
		diags = append(diags, validateSessionEndConsistency(attrs, field)...)
	}
	return diags
}

// IsStringAttributeValue reports whether a string-valued record attribute is
// inside the closed CLI-usage value contract for its key.
func IsStringAttributeValue(key, value string) bool {
	switch key {
	case AttrEventName:
		return IsEvent(value)
	case AttrDeviceID:
		return IsDeviceID(value)
	case AttrCLIVersion:
		return IsCLIVersion(value)
	case AttrOS:
		return IsOS(value)
	case AttrArch:
		return IsArch(value)
	case AttrCommands:
		return IsCommands(value)
	case AttrErrorCategory:
		return IsErrorCategory(value)
	default:
		return false
	}
}

// validateSessionEndConsistency enforces the invariant that a failed session
// carries exactly one error category and a successful one carries none. Missing
// or mistyped success is already reported by the required/kind checks, so this
// only runs when success is a present boolean.
func validateSessionEndConsistency(attrs map[string]telemetry.AnyValue, field string) []diag.Diagnostic {
	success, ok := attrs[AttrSuccess]
	if !ok || success.BoolValue == nil {
		return nil
	}
	_, hasCategory := attrs[AttrErrorCategory]
	at := field + ".attributes." + AttrErrorCategory
	switch {
	case *success.BoolValue && hasCategory:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeSuccessConsistency, at,
			"successful session must not carry an error category")}
	case !*success.BoolValue && !hasCategory:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeSuccessConsistency, at,
			"failed session must carry an error category")}
	}
	return nil
}

func validateResourceService(resource telemetry.Resource, field string) []diag.Diagnostic {
	for _, kv := range resource.Attributes {
		if kv.Key != telemetry.AttrServiceName {
			continue
		}
		if name, ok := stringValue(kv.Value); ok && name == ServiceName {
			return nil
		}
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidService, field+".attributes",
			"service.name must be %q for CLI usage telemetry", ServiceName)}
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidService, field+".attributes",
		"resource is missing the %s attribute", telemetry.AttrServiceName)}
}

// valueKindOf reports the scalar kind an AnyValue carries. It assumes structural
// validity (exactly one kind set); callers run telemetry.ValidateLogsRequest
// first, which rejects multi-kind or empty values.
func valueKindOf(v telemetry.AnyValue) (ValueKind, bool) {
	switch {
	case v.StringValue != nil:
		return KindString, true
	case v.IntValue != nil:
		return KindInt, true
	case v.BoolValue != nil:
		return KindBool, true
	default:
		return 0, false
	}
}

func stringValue(v telemetry.AnyValue) (string, bool) {
	if v.StringValue == nil {
		return "", false
	}
	return *v.StringValue, true
}

func kindName(k ValueKind) string {
	switch k {
	case KindString:
		return "a string value"
	case KindInt:
		return "an integer value"
	case KindBool:
		return "a boolean value"
	default:
		return "an unknown value"
	}
}
