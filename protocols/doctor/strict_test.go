package doctor

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// findCode returns true if diags contains a diagnostic with the given code.
func findCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// validFinding returns a finding that validates clean, so tests can mutate one
// field to isolate a single diagnostic.
func validFinding() Finding {
	return Finding{
		Code:        CheckInsecureTransport,
		Severity:    SeverityHigh,
		Profile:     ProfileProduction,
		Project:     "go.putnami.dev/example/gateway",
		Message:     "plaintext transport",
		Remediation: CheckRemediations[CheckInsecureTransport],
	}
}

// reportWith wraps a single finding in a report with a consistent summary so the
// only diagnostic under test is the one the finding triggers.
func reportWith(f Finding) *Report {
	r := &Report{ProtocolVersion: ProtocolVersion, Profile: ProfileProduction, Findings: []Finding{f}}
	r.Summary = recount(r)
	return r
}

// recount recomputes the summary that validateSummary expects, so summary
// consistency never masks the diagnostic under test.
func recount(r *Report) Summary {
	var s Summary
	s.Findings = len(r.Findings)
	for _, f := range r.Findings {
		switch f.Severity {
		case SeverityInfo:
			s.Info++
		case SeverityWarning:
			s.Warning++
		case SeverityHigh:
			s.High++
		case SeverityCritical:
			s.Critical++
		}
		if f.WaivedBy != nil {
			s.Waived++
		}
	}
	return s
}

func TestParseReport_UnknownField(t *testing.T) {
	r, diags := ParseReport([]byte(`{"protocolVersion":1,"profile":"production","bogus":true}`))
	if r != nil {
		t.Error("report should be nil on parse error")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParseReport_InvalidJSON(t *testing.T) {
	_, diags := ParseReport([]byte("{not json"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
	if diags[0].Code != ErrorCodeParseError {
		t.Errorf("Code = %q, want %s", diags[0].Code, ErrorCodeParseError)
	}
}

func TestParseWaiverFile_UnknownField(t *testing.T) {
	w, diags := ParseWaiverFile([]byte(`{"protocolVersion":1,"bogus":true}`))
	if w != nil {
		t.Error("waiver file should be nil on parse error")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestValidateReport_ProtocolVersionAndProfile(t *testing.T) {
	badVersion := &Report{ProtocolVersion: 99, Profile: ProfileProduction}
	if !findCode(ValidateReport(badVersion), ErrorCodeInvalidProtocolVersion) {
		t.Errorf("want %s", ErrorCodeInvalidProtocolVersion)
	}
	badProfile := &Report{ProtocolVersion: ProtocolVersion, Profile: "staging"}
	if !findCode(ValidateReport(badProfile), ErrorCodeInvalidProfile) {
		t.Errorf("want %s", ErrorCodeInvalidProfile)
	}
}

func TestValidateReport_NilIsParseError(t *testing.T) {
	if !findCode(ValidateReport(nil), ErrorCodeParseError) {
		t.Errorf("want %s for nil report", ErrorCodeParseError)
	}
}

func TestValidateReport_FindingFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Finding)
		code   string
	}{
		{"invalidCheckCode", func(f *Finding) { f.Code = "doctor.nope" }, ErrorCodeInvalidCheckCode},
		{"invalidSeverity", func(f *Finding) { f.Severity = "fatal" }, ErrorCodeInvalidSeverity},
		{"invalidProfile", func(f *Finding) { f.Profile = "staging" }, ErrorCodeInvalidProfile},
		{"missingProject", func(f *Finding) { f.Project = "  " }, ErrorCodeMissingProject},
		{"missingMessage", func(f *Finding) { f.Message = "" }, ErrorCodeMissingMessage},
		{"missingRemediation", func(f *Finding) { f.Remediation = "" }, ErrorCodeMissingRemediation},
		{"emptyEvidence", func(f *Finding) { f.Evidence = []Evidence{{}} }, ErrorCodeInvalidEvidence},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := validFinding()
			tc.mutate(&f)
			diags := ValidateReport(reportWith(f))
			if !findCode(diags, tc.code) {
				t.Errorf("want %s, got %v", tc.code, diags)
			}
		})
	}
}

func TestValidateReport_WaivedByProvenance(t *testing.T) {
	cases := []struct {
		name string
		prov WaiverProvenance
		code string
	}{
		{"missingOwner", WaiverProvenance{Reason: "r", Expires: "2027-01-01"}, ErrorCodeMissingOwner},
		{"missingReason", WaiverProvenance{Owner: "o", Expires: "2027-01-01"}, ErrorCodeMissingReason},
		{"badExpires", WaiverProvenance{Owner: "o", Reason: "r", Expires: "soon"}, ErrorCodeInvalidExpires},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := validFinding()
			f.WaivedBy = &tc.prov
			diags := ValidateReport(reportWith(f))
			if !findCode(diags, tc.code) {
				t.Errorf("want %s, got %v", tc.code, diags)
			}
		})
	}
}

func TestValidateReport_SummaryMismatch(t *testing.T) {
	r := &Report{
		ProtocolVersion: ProtocolVersion,
		Profile:         ProfileProduction,
		Findings:        []Finding{validFinding()},
		Summary:         Summary{Findings: 5, High: 5},
	}
	if !findCode(ValidateReport(r), ErrorCodeInvalidSummary) {
		t.Errorf("want %s for a lying summary", ErrorCodeInvalidSummary)
	}
}

func TestValidateReport_SummaryAgrees(t *testing.T) {
	r := sampleReport()
	if diags := ValidateReport(r); diag.HasErrors(diags) {
		t.Errorf("sampleReport should validate clean, got %v", diags)
	}
}

func TestValidateWaiverFile_WaiverFields(t *testing.T) {
	valid := func() Waiver {
		return Waiver{Code: CheckLocalRateLimit, Owner: "o", Reason: "r", Expires: "2027-01-01"}
	}
	cases := []struct {
		name   string
		mutate func(*Waiver)
		code   string
	}{
		{"invalidCheckCode", func(w *Waiver) { w.Code = "doctor.nope" }, ErrorCodeInvalidCheckCode},
		{"missingOwner", func(w *Waiver) { w.Owner = "" }, ErrorCodeMissingOwner},
		{"missingReason", func(w *Waiver) { w.Reason = "" }, ErrorCodeMissingReason},
		{"badExpires", func(w *Waiver) { w.Expires = "next week" }, ErrorCodeInvalidExpires},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wv := valid()
			tc.mutate(&wv)
			diags := ValidateWaiverFile(&WaiverFile{ProtocolVersion: ProtocolVersion, Waivers: []Waiver{wv}})
			if !findCode(diags, tc.code) {
				t.Errorf("want %s, got %v", tc.code, diags)
			}
		})
	}
}

func TestValidateWaiverFile_NilIsParseError(t *testing.T) {
	if !findCode(ValidateWaiverFile(nil), ErrorCodeParseError) {
		t.Errorf("want %s for nil waiver file", ErrorCodeParseError)
	}
}

func TestValidateWaiverFile_DuplicateScopedWaiver(t *testing.T) {
	w := &WaiverFile{
		ProtocolVersion: ProtocolVersion,
		Waivers: []Waiver{
			{Code: CheckVolatilePersistence, Project: "p", Owner: "o", Reason: "r", Expires: "2027-01-01"},
			{Code: CheckVolatilePersistence, Project: "p", Owner: "o2", Reason: "r2", Expires: "2027-02-01"},
		},
	}
	if !findCode(ValidateWaiverFile(w), ErrorCodeDuplicateWaiver) {
		t.Errorf("want %s for duplicate (code, project) waiver", ErrorCodeDuplicateWaiver)
	}
}

func TestValidateWaiverFile_DuplicateWorkspaceWideWaiver(t *testing.T) {
	w := &WaiverFile{
		ProtocolVersion: ProtocolVersion,
		Waivers: []Waiver{
			{Code: CheckLocalRateLimit, Owner: "o", Reason: "r", Expires: "2027-01-01"},
			{Code: CheckLocalRateLimit, Owner: "o2", Reason: "r2", Expires: "2027-02-01"},
		},
	}
	if !findCode(ValidateWaiverFile(w), ErrorCodeDuplicateWaiver) {
		t.Errorf("want %s for two workspace-wide waivers of the same code", ErrorCodeDuplicateWaiver)
	}
}

func TestValidateWaiverFile_SameCodeDifferentProjectIsNotDuplicate(t *testing.T) {
	w := &WaiverFile{
		ProtocolVersion: ProtocolVersion,
		Waivers: []Waiver{
			{Code: CheckVolatilePersistence, Project: "a", Owner: "o", Reason: "r", Expires: "2027-01-01"},
			{Code: CheckVolatilePersistence, Project: "b", Owner: "o", Reason: "r", Expires: "2027-01-01"},
		},
	}
	if findCode(ValidateWaiverFile(w), ErrorCodeDuplicateWaiver) {
		t.Errorf("same code scoped to different projects must not be a duplicate: %v", ValidateWaiverFile(w))
	}
}

func TestValidateWaiverFile_SameCodeAndProjectDifferentFieldIsNotDuplicate(t *testing.T) {
	// The whole point of field scoping: accepting several findings of one code
	// in one project one field at a time must be allowed, not rejected as a
	// duplicate.
	w := &WaiverFile{
		ProtocolVersion: ProtocolVersion,
		Waivers: []Waiver{
			{Code: CheckMissingRequiredConfig, Project: "p", Field: "server.port", Owner: "o", Reason: "r", Expires: "2027-01-01"},
			{Code: CheckMissingRequiredConfig, Project: "p", Field: "database.password", Owner: "o", Reason: "r", Expires: "2027-01-01"},
		},
	}
	if findCode(ValidateWaiverFile(w), ErrorCodeDuplicateWaiver) {
		t.Errorf("same code+project scoped to different fields must not be a duplicate: %v", ValidateWaiverFile(w))
	}
}

func TestValidateWaiverFile_SameCodeProjectAndFieldIsDuplicate(t *testing.T) {
	w := &WaiverFile{
		ProtocolVersion: ProtocolVersion,
		Waivers: []Waiver{
			{Code: CheckMissingRequiredConfig, Project: "p", Field: "server.port", Owner: "o", Reason: "r", Expires: "2027-01-01"},
			{Code: CheckMissingRequiredConfig, Project: "p", Field: "server.port", Owner: "o2", Reason: "r2", Expires: "2027-02-01"},
		},
	}
	if !findCode(ValidateWaiverFile(w), ErrorCodeDuplicateWaiver) {
		t.Errorf("want %s for two waivers of the same code+project+field", ErrorCodeDuplicateWaiver)
	}
}

func TestValidExpires_AcceptsDateAndTimestamp(t *testing.T) {
	ok := []string{"2027-06-30", "2027-06-30T12:00:00Z", "2027-06-30T12:00:00+02:00"}
	for _, s := range ok {
		if !validExpires(s) {
			t.Errorf("validExpires(%q) = false, want true", s)
		}
	}
	bad := []string{"", "  ", "soon", "30-06-2027", "2027/06/30"}
	for _, s := range bad {
		if validExpires(s) {
			t.Errorf("validExpires(%q) = true, want false", s)
		}
	}
}

func TestParseAndValidateReport_StopsAtParseError(t *testing.T) {
	r, diags := ParseAndValidateReport([]byte(`{"protocolVersion":1,"profile":"production","bogus":1}`))
	if r != nil {
		t.Error("report should be nil when parsing fails")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParseAndValidateWaiverFile_StopsAtParseError(t *testing.T) {
	w, diags := ParseAndValidateWaiverFile([]byte("{not json"))
	if w != nil {
		t.Error("waiver file should be nil when parsing fails")
	}
	if !findCode(diags, ErrorCodeParseError) {
		t.Errorf("want %s, got %v", ErrorCodeParseError, diags)
	}
}

// TestCheckCodeTaxonomy_Frozen pins the frozen check-code taxonomy: every
// required ID is present, every code has a baked remediation, and the two maps
// agree. A committed doctor.waivers.json names these IDs and fails closed
// against a parser that no longer knows one, so a drop or rename here breaks
// every repository that already accepted a finding — see
// doc/adr/0001-frozen-check-code-taxonomy.md.
func TestCheckCodeTaxonomy_Frozen(t *testing.T) {
	required := []CheckCode{
		CheckIncompleteCapability,
		CheckMissingRequiredConfig,
		CheckVolatilePersistence,
		CheckEphemeralSigningKey,
		CheckInsecureTransport,
		CheckLocalRateLimit,
		CheckInvalidGeneratedSchema,
		CheckConfigShadowing,
		CheckWaiverExpired,
		CheckWaiverUnknownCode,
		CheckCommittedManifestStability,
		CheckUndeclaredSchemaCommit,
		CheckCRLFCheckout,
		CheckLongPathsDisabled,
		CheckGitLongPathsDisabled,
		CheckVCRuntimeMissing,
		CheckMissingReadme,
	}
	for _, code := range required {
		if !ValidCheckCodes[code] {
			t.Errorf("ValidCheckCodes missing %q", code)
		}
		if CheckRemediations[code] == "" {
			t.Errorf("CheckRemediations missing a baked remediation for %q", code)
		}
		if Remediation(code) != CheckRemediations[code] {
			t.Errorf("Remediation(%q) disagrees with CheckRemediations", code)
		}
	}
	if len(ValidCheckCodes) != len(required) {
		t.Errorf("ValidCheckCodes has %d entries, want %d — update this test if extending the taxonomy (needs a protocol bump)", len(ValidCheckCodes), len(required))
	}
	if len(CheckRemediations) != len(required) {
		t.Errorf("CheckRemediations has %d entries, want %d", len(CheckRemediations), len(required))
	}
	if Remediation("doctor.unknown") != "" {
		t.Error("Remediation of an unknown code should be empty")
	}
}

// TestDiagnosticTaxonomy_Membership asserts every constant is registered in
// ValidDiagnosticCodes so tooling can trust the map is exhaustive.
func TestDiagnosticTaxonomy_Membership(t *testing.T) {
	required := []string{
		ErrorCodeParseError,
		ErrorCodeUnknownField,
		ErrorCodeInvalidProtocolVersion,
		ErrorCodeInvalidSeverity,
		ErrorCodeInvalidProfile,
		ErrorCodeInvalidCheckCode,
		ErrorCodeMissingProject,
		ErrorCodeMissingMessage,
		ErrorCodeMissingRemediation,
		ErrorCodeInvalidEvidence,
		ErrorCodeInvalidSummary,
		ErrorCodeMissingOwner,
		ErrorCodeMissingReason,
		ErrorCodeInvalidExpires,
		ErrorCodeDuplicateWaiver,
	}
	for _, code := range required {
		if !ValidDiagnosticCodes[code] {
			t.Errorf("ValidDiagnosticCodes missing %q", code)
		}
	}
	if len(ValidDiagnosticCodes) != len(required) {
		t.Errorf("ValidDiagnosticCodes has %d entries, want %d", len(ValidDiagnosticCodes), len(required))
	}
}

// TestEnums_ValuesMatchMaps keeps the ordered value slices and the Valid* maps
// in agreement.
func TestEnums_ValuesMatchMaps(t *testing.T) {
	if len(SeverityValues) != len(ValidSeverities) {
		t.Errorf("SeverityValues/ValidSeverities length mismatch: %d vs %d", len(SeverityValues), len(ValidSeverities))
	}
	for _, s := range SeverityValues {
		if !ValidSeverities[s] {
			t.Errorf("SeverityValues has %q but ValidSeverities does not", s)
		}
	}
	if len(ProfileValues) != len(ValidProfiles) {
		t.Errorf("ProfileValues/ValidProfiles length mismatch: %d vs %d", len(ProfileValues), len(ValidProfiles))
	}
	for _, p := range ProfileValues {
		if !ValidProfiles[p] {
			t.Errorf("ProfileValues has %q but ValidProfiles does not", p)
		}
	}
}

// TestWaiverFilename pins the committed waiver file name; the CLI engine reads
// exactly this path at the workspace root.
func TestWaiverFilename(t *testing.T) {
	if WaiverFilename != "doctor.waivers.json" {
		t.Errorf("WaiverFilename = %q, want doctor.waivers.json", WaiverFilename)
	}
}
