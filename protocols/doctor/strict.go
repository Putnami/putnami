package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Diagnostic error codes for strict parsing and validation. These are the
// parse/validate taxonomy — distinct from the ValidCheckCodes findings
// vocabulary in doctor.go. Both are namespaced "doctor.*"; tooling keys off
// these, so keep them in sync with ValidDiagnosticCodes below.
const (
	ErrorCodeParseError             = "doctor.parse_error"
	ErrorCodeUnknownField           = "doctor.unknown_field"
	ErrorCodeInvalidProtocolVersion = "doctor.invalid_protocol_version"
	ErrorCodeInvalidSeverity        = "doctor.invalid_severity"
	ErrorCodeInvalidProfile         = "doctor.invalid_profile"
	ErrorCodeInvalidCheckCode       = "doctor.invalid_check_code"
	ErrorCodeMissingProject         = "doctor.missing_project"
	ErrorCodeMissingMessage         = "doctor.missing_message"
	ErrorCodeMissingRemediation     = "doctor.missing_remediation"
	ErrorCodeInvalidEvidence        = "doctor.invalid_evidence"
	ErrorCodeInvalidSummary         = "doctor.invalid_summary"
	ErrorCodeMissingOwner           = "doctor.missing_owner"
	ErrorCodeMissingReason          = "doctor.missing_reason"
	ErrorCodeInvalidExpires         = "doctor.invalid_expires"
	ErrorCodeDuplicateWaiver        = "doctor.duplicate_waiver"
)

// ValidDiagnosticCodes enumerates the canonical doctor parse/validate error
// taxonomy. It is distinct from ValidCheckCodes (findings vocabulary): a
// diagnostic describes a malformed Report or WaiverFile, while a check code
// names a class of production-readiness finding.
var ValidDiagnosticCodes = map[string]bool{
	ErrorCodeParseError:             true,
	ErrorCodeUnknownField:           true,
	ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeInvalidSeverity:        true,
	ErrorCodeInvalidProfile:         true,
	ErrorCodeInvalidCheckCode:       true,
	ErrorCodeMissingProject:         true,
	ErrorCodeMissingMessage:         true,
	ErrorCodeMissingRemediation:     true,
	ErrorCodeInvalidEvidence:        true,
	ErrorCodeInvalidSummary:         true,
	ErrorCodeMissingOwner:           true,
	ErrorCodeMissingReason:          true,
	ErrorCodeInvalidExpires:         true,
	ErrorCodeDuplicateWaiver:        true,
}

// ParseReport decodes a doctor Report from JSON in strict mode (unknown fields
// rejected). It returns a non-nil report only when decoding produced no errors.
func ParseReport(data []byte) (*Report, []diag.Diagnostic) {
	var r Report
	if d := strictDecode(data, &r); d != nil {
		return nil, d
	}
	return &r, nil
}

// ParseWaiverFile decodes a doctor WaiverFile from JSON in strict mode (unknown
// fields rejected). It returns a non-nil file only when decoding produced no
// errors.
func ParseWaiverFile(data []byte) (*WaiverFile, []diag.Diagnostic) {
	var w WaiverFile
	if d := strictDecode(data, &w); d != nil {
		return nil, d
	}
	return &w, nil
}

// strictDecode runs a DisallowUnknownFields decode and maps a decode failure to
// a coded diagnostic. It returns nil on success.
func strictDecode(data []byte, v any) []diag.Diagnostic {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		code := ErrorCodeParseError
		field := ""
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field ") {
			code = ErrorCodeUnknownField
			field = strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		}
		return []diag.Diagnostic{diag.Errorf(code, field, "%s", msg)}
	}
	return nil
}

// ValidateReport checks structural and semantic invariants on a parsed report.
// It returns one diagnostic per finding in a stable, source-order sequence:
// protocol version, report profile, each finding in order, then summary
// consistency.
func ValidateReport(r *Report) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "report is nil")}
	}

	var diags []diag.Diagnostic
	diags = append(diags, validateProtocolVersion(r.ProtocolVersion)...)
	if !ValidProfiles[r.Profile] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProfile, "profile",
			"report profile %q is not in the v1 set: dev, test, production", r.Profile))
	}

	for i, f := range r.Findings {
		field := fmt.Sprintf("findings[%d]", i)
		if !ValidCheckCodes[f.Code] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidCheckCode, field+".code",
				"check code %q is not in the v1 taxonomy; use a code from ValidCheckCodes", f.Code))
		}
		if !ValidSeverities[f.Severity] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSeverity, field+".severity",
				"severity %q is not in the v1 set: info, warning, high, critical", f.Severity))
		}
		if !ValidProfiles[f.Profile] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProfile, field+".profile",
				"finding profile %q is not in the v1 set: dev, test, production", f.Profile))
		}
		if strings.TrimSpace(f.Project) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingProject, field+".project",
				"finding must name the affected project; set project to a workspace-relative path or project id"))
		}
		if strings.TrimSpace(f.Message) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingMessage, field+".message",
				"finding must carry a human-readable message"))
		}
		if strings.TrimSpace(f.Remediation) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingRemediation, field+".remediation",
				"finding must carry an actionable remediation; stamp doctor.Remediation(code)"))
		}
		for j, e := range f.Evidence {
			if strings.TrimSpace(e.Path) == "" && strings.TrimSpace(e.Field) == "" {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidEvidence,
					fmt.Sprintf("%s.evidence[%d]", field, j),
					"evidence must name a path or a field; drop the empty entry"))
			}
		}
		diags = append(diags, validateWaiverProvenance(field+".waivedBy", f.WaivedBy)...)
	}

	// Semantic pass runs after the source-order structural pass so callers see
	// malformed findings first, followed by the deterministic summary check.
	diags = append(diags, validateSummary(r)...)
	return diags
}

// validateWaiverProvenance checks a finding's optional WaivedBy block. When
// present it must carry an owner, a reason, and a parsable expiry. Expiry is
// never compared to a clock here (see the package doc).
func validateWaiverProvenance(field string, p *WaiverProvenance) []diag.Diagnostic {
	if p == nil {
		return nil
	}
	var diags []diag.Diagnostic
	if strings.TrimSpace(p.Owner) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingOwner, field+".owner",
			"waiver provenance must record an owner"))
	}
	if strings.TrimSpace(p.Reason) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingReason, field+".reason",
			"waiver provenance must record a reason"))
	}
	if !validExpires(p.Expires) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidExpires, field+".expires",
			"expires %q is not an RFC 3339 date (YYYY-MM-DD) or timestamp; fix the value", p.Expires))
	}
	return diags
}

// validateSummary recomputes the finding tallies and rejects a summary that
// disagrees with the findings. A report whose summary lies is a correctness
// bug: the summary is a deterministic function of the findings, so it must be
// reproducible from them.
func validateSummary(r *Report) []diag.Diagnostic {
	var want Summary
	want.Findings = len(r.Findings)
	for _, f := range r.Findings {
		switch f.Severity {
		case SeverityInfo:
			want.Info++
		case SeverityWarning:
			want.Warning++
		case SeverityHigh:
			want.High++
		case SeverityCritical:
			want.Critical++
		}
		if f.WaivedBy != nil {
			want.Waived++
		}
	}
	if r.Summary == want {
		return nil
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidSummary, "summary",
		"summary %+v does not match the findings (want %+v); recompute the counts", r.Summary, want)}
}

// ValidateWaiverFile checks structural and semantic invariants on a parsed
// waiver file: protocol version, each waiver in source order (known check code,
// non-empty owner and reason, parsable expiry), then duplicate detection.
func ValidateWaiverFile(w *WaiverFile) []diag.Diagnostic {
	if w == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "waiver file is nil")}
	}

	var diags []diag.Diagnostic
	diags = append(diags, validateProtocolVersion(w.ProtocolVersion)...)

	for i, wv := range w.Waivers {
		field := fmt.Sprintf("waivers[%d]", i)
		if !ValidCheckCodes[wv.Code] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidCheckCode, field+".code",
				"check code %q is not in the v1 taxonomy; waive a code from ValidCheckCodes", wv.Code))
		}
		if strings.TrimSpace(wv.Owner) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingOwner, field+".owner",
				"waiver must record an owner"))
		}
		if strings.TrimSpace(wv.Reason) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingReason, field+".reason",
				"waiver must record a reason"))
		}
		if !validExpires(wv.Expires) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidExpires, field+".expires",
				"expires %q is not an RFC 3339 date (YYYY-MM-DD) or timestamp; fix the value", wv.Expires))
		}
	}

	// Semantic pass: reject two waivers that scope the same check code to the
	// same project. The second occurrence is reported so the message is stable.
	diags = append(diags, validateDuplicateWaivers(w.Waivers)...)
	return diags
}

// validateDuplicateWaivers rejects duplicate (code, project, field) waivers. Two
// waivers that target the same check code, the same project scope (both empty
// scopes count as the same workspace-wide scope), AND the same field are
// ambiguous: which owner/reason/expiry wins is undefined. Waivers that share a
// code and project but scope to different fields are distinct and allowed, so an
// author can accept several findings of one code one field at a time.
func validateDuplicateWaivers(waivers []Waiver) []diag.Diagnostic {
	seen := make(map[string]int, len(waivers))
	var diags []diag.Diagnostic
	for i, wv := range waivers {
		key := string(wv.Code) + "\x00" + wv.Project + "\x00" + wv.Field
		if first, ok := seen[key]; ok {
			scope := "workspace-wide"
			if strings.TrimSpace(wv.Project) != "" {
				scope = fmt.Sprintf("project %q", wv.Project)
			}
			if strings.TrimSpace(wv.Field) != "" {
				scope += fmt.Sprintf(" field %q", wv.Field)
			}
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateWaiver,
				fmt.Sprintf("waivers[%d]", i),
				"check code %q is waived more than once for %s (also waivers[%d]); keep a single waiver",
				wv.Code, scope, first))
			continue
		}
		seen[key] = i
	}
	return diags
}

// validateProtocolVersion checks that v is the supported protocol version.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
	}
	return nil
}

// validExpires reports whether s parses as an RFC 3339 date (YYYY-MM-DD) or a
// full RFC 3339 timestamp. It deliberately performs no clock comparison: this
// package validates shape only, and expiry evaluation lives in the CLI engine
// with an injected clock.
func validExpires(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return true
	}
	if _, err := time.Parse(time.DateOnly, s); err == nil {
		return true
	}
	return false
}

// ParseAndValidateReport is a convenience that runs strict parsing followed by
// structural and semantic validation.
func ParseAndValidateReport(data []byte) (*Report, []diag.Diagnostic) {
	r, diags := ParseReport(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return r, append(diags, ValidateReport(r)...)
}

// ParseAndValidateWaiverFile is a convenience that runs strict parsing followed
// by structural and semantic validation.
func ParseAndValidateWaiverFile(data []byte) (*WaiverFile, []diag.Diagnostic) {
	w, diags := ParseWaiverFile(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return w, append(diags, ValidateWaiverFile(w)...)
}
