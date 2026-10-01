package runtime

import (
	"fmt"

	diag "go.putnami.dev/protocol/diagnostic"
)

// validEventTypes is the set of known event types.
var validEventTypes = map[EventType]bool{
	EventLog:        true,
	EventProgress:   true,
	EventArtifact:   true,
	EventDiagnostic: true,
	EventMetric:     true,
	EventPhase:      true,
	EventSummary:    true,
	EventResult:     true,
	EventMeta:       true,
}

// ValidateEvent checks structural invariants on a single parsed event.
func ValidateEvent(e *Event) []diag.Diagnostic {
	if e == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-event", "", "event is nil"),
		}
	}

	var diags []diag.Diagnostic

	if !IsKnownProtocolVersion(e.V) {
		diags = append(diags, diag.Errorf("invalid-version", "v",
			"expected protocol version %d or %d, got %d", ProtocolVersion, ProtocolVersion2, e.V))
	}

	// The admitted type vocabulary is version-scoped: v1 admits exactly the
	// nine v1 types and v2 adds ready. A ready event stamped v1 is therefore an
	// unknown type, which is what keeps v2 additive instead of retroactively
	// widening v1.
	if !eventTypesForVersion(e.V)[e.Type] {
		diags = append(diags, diag.Errorf("invalid-event-type", "type",
			"unknown event type %q", e.Type))
		return diags // can't validate further without known type
	}

	switch e.Type {
	case EventLog:
		diags = append(diags, validateLogEvent(e)...)
	case EventProgress:
		diags = append(diags, validateProgressEvent(e)...)
	case EventDiagnostic:
		diags = append(diags, validateDiagnosticEvent(e)...)
	case EventMetric:
		diags = append(diags, validateMetricEvent(e)...)
	case EventPhase:
		diags = append(diags, validatePhaseEvent(e)...)
	case EventArtifact:
		diags = append(diags, validateArtifactEvent(e)...)
	case EventSummary:
		diags = append(diags, validateSummaryEvent(e)...)
	case EventReady:
		diags = append(diags, validateReadyEvent(e)...)
	}

	return diags
}

// ValidateEventStream checks that a sequence of events forms a valid stream.
// It verifies exactly one result event exists and appears last, and that the
// whole stream speaks one protocol version.
func ValidateEventStream(events []*Event) []diag.Diagnostic {
	var diags []diag.Diagnostic
	resultCount := 0
	lastResultIdx := -1
	versions := map[int]bool{}

	for i, e := range events {
		eDiags := ValidateEvent(e)
		for j := range eDiags {
			eDiags[j].Field = fmt.Sprintf("event[%d].%s", i, eDiags[j].Field)
		}
		diags = append(diags, eDiags...)

		versions[e.V] = true
		if e.Type == EventResult {
			resultCount++
			lastResultIdx = i
		}
	}

	// One subprocess emits one contract. A stream that changes version
	// mid-flight cannot be read as either version, so it is rejected rather
	// than interpreted per line. An all-v1 stream — every stream emitted today
	// — never triggers this.
	if len(versions) > 1 {
		diags = append(diags, diag.Errorf("mixed-protocol-version", "",
			"event stream mixes protocol versions %s", sortedVersions(versions)))
	}

	if resultCount == 0 {
		diags = append(diags, diag.Errorf("missing-result", "",
			"event stream must contain exactly one result event"))
	} else if resultCount > 1 {
		diags = append(diags, diag.Errorf("multiple-results", "",
			"event stream contains %d result events, expected exactly one", resultCount))
	}

	if resultCount == 1 && lastResultIdx != len(events)-1 {
		diags = append(diags, diag.Warningf("result-not-last", "",
			"result event should be the last event in the stream"))
	}

	return diags
}

func validateLogEvent(e *Event) []diag.Diagnostic {
	var diags []diag.Diagnostic
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if e.Level == "" {
		diags = append(diags, diag.Errorf("required-field", "level", "log event requires level"))
	} else if !validLevels[e.Level] {
		diags = append(diags, diag.Errorf("invalid-enum", "level",
			"invalid log level %q; must be one of: debug, info, warn, error", e.Level))
	}
	if e.Message == "" {
		diags = append(diags, diag.Errorf("required-field", "message", "log event requires message"))
	}
	return diags
}

func validateProgressEvent(e *Event) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if e.Current == nil {
		diags = append(diags, diag.Errorf("required-field", "current", "progress event requires current"))
	}
	if e.Total == nil {
		diags = append(diags, diag.Errorf("required-field", "total", "progress event requires total"))
	}
	return diags
}

func validateDiagnosticEvent(e *Event) []diag.Diagnostic {
	var diags []diag.Diagnostic
	validSeverities := map[DiagnosticSeverity]bool{
		SeverityError: true, SeverityWarning: true, SeverityInfo: true, SeverityHint: true,
	}
	if e.Severity == nil {
		diags = append(diags, diag.Errorf("required-field", "severity", "diagnostic event requires severity"))
	} else if !validSeverities[*e.Severity] {
		diags = append(diags, diag.Errorf("invalid-enum", "severity",
			"invalid diagnostic severity %q", *e.Severity))
	}
	if e.Message == "" {
		diags = append(diags, diag.Errorf("required-field", "message", "diagnostic event requires message"))
	}
	return diags
}

func validateMetricEvent(e *Event) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if e.Name == "" {
		diags = append(diags, diag.Errorf("required-field", "name", "metric event requires name"))
	}
	if e.Value == nil {
		diags = append(diags, diag.Errorf("required-field", "value", "metric event requires value"))
	}
	return diags
}

func validatePhaseEvent(e *Event) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if e.Name == "" {
		diags = append(diags, diag.Errorf("required-field", "name", "phase event requires name"))
	}
	if e.Action == nil {
		diags = append(diags, diag.Errorf("required-field", "action", "phase event requires action"))
	} else {
		validActions := map[PhaseAction]bool{PhaseStart: true, PhaseEnd: true}
		if !validActions[*e.Action] {
			diags = append(diags, diag.Errorf("invalid-enum", "action",
				"invalid phase action %q; must be start or end", *e.Action))
		}
	}
	return diags
}

func validateArtifactEvent(e *Event) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if e.ID == "" {
		diags = append(diags, diag.Errorf("required-field", "id", "artifact event requires id"))
	}
	if e.Name == "" {
		diags = append(diags, diag.Errorf("required-field", "name", "artifact event requires name"))
	}
	if e.Kind == "" {
		diags = append(diags, diag.Errorf("required-field", "kind", "artifact event requires kind"))
	}
	if e.Path == "" {
		diags = append(diags, diag.Errorf("required-field", "path", "artifact event requires path"))
	}
	return diags
}

func validateSummaryEvent(e *Event) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if e.Message == "" {
		diags = append(diags, diag.Errorf("required-field", "message", "summary event requires message"))
	}
	return diags
}
