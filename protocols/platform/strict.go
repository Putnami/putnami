package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Platform error codes for strict validation failures. Tooling and
// runtime compliance suites key off these — keep them in sync with
// ValidErrorCodes below.
const (
	ErrorCodeInvalidProbeName    = "platform.invalid_probe_name"
	ErrorCodeDuplicateProbe      = "platform.duplicate_probe"
	ErrorCodeInvalidStatus       = "platform.invalid_status"
	ErrorCodeInvalidEndpoint     = "platform.invalid_endpoint"
	ErrorCodeInvalidEnvelope     = "platform.invalid_envelope"
	ErrorCodeInvalidCapability   = "platform.invalid_capability"
	ErrorCodeInvalidPrefix       = "platform.invalid_prefix"
	ErrorCodeProbeTimeout        = "platform.probe_timeout"
	ErrorCodeProbeContractBroken = "platform.probe_contract_broken"
)

// ValidErrorCodes enumerates the canonical platform error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeInvalidProbeName:    true,
	ErrorCodeDuplicateProbe:      true,
	ErrorCodeInvalidStatus:       true,
	ErrorCodeInvalidEndpoint:     true,
	ErrorCodeInvalidEnvelope:     true,
	ErrorCodeInvalidCapability:   true,
	ErrorCodeInvalidPrefix:       true,
	ErrorCodeProbeTimeout:        true,
	ErrorCodeProbeContractBroken: true,
}

// ErrorCodeMissingProbe labels the required-readiness behavior: a probe
// name a workload marks as required that was never registered or
// auto-discovered. Unlike the codes above it is NOT a strict-validation
// diagnostic — no validator emits it and it changes no wire shape. The
// runtime expresses a missing required probe through the EXISTING
// degraded envelope (a synthesized failing checks entry keyed by the
// missing name), so it needs no protocol bump. It is therefore
// deliberately absent from ValidErrorCodes (which enumerates only the
// codes ValidateEnvelope and the other strict validators produce); it
// exists as a symbol so tooling and cross-language compliance suites
// reference the taxonomy label instead of hardcoding the string.
const ErrorCodeMissingProbe = "platform.missing_probe"

// probeNamePattern matches a canonical probe identifier. Probe names
// show up in JSON keys, log lines, and metric labels, so we keep them
// to a sane alphabet: lowercase letters, digits, '-', '_', '.', '/'.
// 1–64 chars; runtimes that emit names outside this set are
// non-conforming.
var probeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_./-]{0,63}$`)

// prefixTrimSet is what NormalizePrefix drops at either end: ASCII
// whitespace and "/". It is an explicit set rather than unicode.IsSpace so
// the TypeScript mirror trims exactly the same characters.
const prefixTrimSet = " \t\n\v\f\r/"

// NormalizePrefix collapses the prefix to canonical form: "" or
// "/segment[/...]". ASCII whitespace and slashes at either end are dropped
// together, then one leading slash is added: "", "/", "//" and "/ /" all
// normalise to "", and "v1", " v1", "//v1", "/ v1" and "/v1/ " all
// normalise to "/v1". Interior characters, a doubled slash included, are
// kept as written. The protocol requires runtimes to apply these rules
// before mounting, so two workloads with equivalent prefixes mount on
// identical paths. Every Go route prefix (api WithPrefix, grpc PathPrefix,
// platform Prefix) goes through it.
func NormalizePrefix(raw string) string {
	trimmed := strings.Trim(raw, prefixTrimSet)
	if trimmed == "" {
		return ""
	}
	return "/" + trimmed
}

// ValidateProbeName checks that name follows the canonical probe-name
// rules. Returns a single diagnostic on failure, nil otherwise.
func ValidateProbeName(name string) []diag.Diagnostic {
	if name == "" {
		return []diag.Diagnostic{diag.Errorf(
			ErrorCodeInvalidProbeName, "name", "probe name is required",
		)}
	}
	if !probeNamePattern.MatchString(name) {
		return []diag.Diagnostic{diag.Errorf(
			ErrorCodeInvalidProbeName, "name",
			"probe name %q does not match canonical pattern %q", name, probeNamePattern.String(),
		)}
	}
	return nil
}

// ValidateStatus checks that s is one of the canonical envelope status
// values. Returns a single diagnostic on failure, nil otherwise.
func ValidateStatus(s Status) []diag.Diagnostic {
	if !ValidStatuses[s] {
		return []diag.Diagnostic{diag.Errorf(
			ErrorCodeInvalidStatus, "status",
			"status %q is not a canonical platform status", s,
		)}
	}
	return nil
}

// ValidateEnvelope validates a canonical response envelope.
//
// Rules:
//   - Status is required and must be a canonical value.
//   - When Status is "ok" or "degraded", Checks may contain entries; every
//     entry value is either "ok" or a non-empty error string.
//   - When Status is "unavailable", Checks must be empty (the runtime
//     short-circuited before running probes).
//   - When Status is "degraded", at least one Checks entry must be a
//     non-"ok" failure string — otherwise the runtime is reporting
//     degraded with no failing probe, which is a contract violation.
func ValidateEnvelope(e Envelope) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, ValidateStatus(e.Status)...)

	switch e.Status {
	case StatusUnavailable:
		if len(e.Checks) != 0 {
			diags = append(diags, diag.Errorf(
				ErrorCodeInvalidEnvelope, "checks",
				"envelope with status %q must not include checks", StatusUnavailable,
			))
		}
	case StatusDegraded:
		hasFailure := false
		for name, entry := range e.Checks {
			if nameDiags := ValidateProbeName(name); len(nameDiags) > 0 {
				diags = append(diags, nameDiags...)
			}
			if entry != "ok" && entry != "" {
				hasFailure = true
			}
		}
		if !hasFailure {
			diags = append(diags, diag.Errorf(
				ErrorCodeInvalidEnvelope, "checks",
				"envelope with status %q must include at least one failing probe", StatusDegraded,
			))
		}
	case StatusOK:
		for name, entry := range e.Checks {
			if nameDiags := ValidateProbeName(name); len(nameDiags) > 0 {
				diags = append(diags, nameDiags...)
			}
			if entry != "ok" {
				diags = append(diags, diag.Errorf(
					ErrorCodeInvalidEnvelope, fmt.Sprintf("checks[%s]", name),
					"envelope with status %q must report %q for every check; got %q",
					StatusOK, "ok", entry,
				))
			}
		}
	}

	return diags
}

// ParseEnvelope decodes a canonical response envelope from JSON in
// strict mode (unknown fields rejected). It returns a non-nil envelope
// only when parsing produced no errors.
func ParseEnvelope(data []byte) (*Envelope, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var e Envelope
	if err := dec.Decode(&e); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(
			ErrorCodeInvalidEnvelope, "",
			"invalid envelope JSON: %v", err,
		)}
	}
	return &e, nil
}

// ParseAndValidateEnvelope runs strict parsing followed by envelope
// validation. Cross-language compliance suites feed the shared fixture
// corpus through this entry point.
func ParseAndValidateEnvelope(data []byte) (*Envelope, []diag.Diagnostic) {
	e, diags := ParseEnvelope(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return e, append(diags, ValidateEnvelope(*e)...)
}

// HTTPStatusFor returns the canonical HTTP status code for an envelope
// status. Runtimes must use these values exactly — operators key on
// 200 vs 503 in their alerting.
func HTTPStatusFor(s Status) (int, bool) {
	switch s {
	case StatusOK:
		return HTTPStatusOK, true
	case StatusUnavailable, StatusDegraded:
		return HTTPStatusUnavailable, true
	default:
		return 0, false
	}
}
