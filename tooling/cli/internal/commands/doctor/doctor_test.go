package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestCheckCapabilities_V2PreservesMissingProviderClassification(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "schema", "capabilities.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "protocolVersion": 2,
  "project": "example/app",
  "requiredCapabilities": [{
    "identity": {"ownerProject":"example/app","kind":"requiredCapability","key":"configured"},
    "name": "configured",
    "requires": ["config"],
    "provenance": {
      "project": "example/app",
      "sourceKind": "manual",
      "declaration": {"root":"project","path":"requirements.go"}
    }
  }]
}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	findings := checkCapabilities(doctorProject{absDir: dir, relPath: "app", id: "/app", profile: doctor.ProfileProduction})
	if len(findings) != 1 || findings[0].Code != doctor.CheckIncompleteCapability || findings[0].Evidence[0].Field != "requiredCapabilities[0].requires[0]" {
		t.Fatalf("v2 findings = %#v, want one incomplete-capability classification", findings)
	}
}

// doctorTestdataRoot is the fixture workspace root; each subdirectory is a
// project holding only committed artifacts (schema/*.json, conf/.env*.yaml).
const doctorTestdataRoot = "testdata/doctor"

// doctorTestClock is a fixed, pinned clock for the non-waiver doctor tests.
// Those fixture roots carry no doctor.waivers.json, so expiry is never
// evaluated; the value only has to be deterministic. Waiver tests pin their own
// clocks per case (see doctor_waivers_test.go).
var doctorTestClock = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

// proj builds a minimal workspace project pointing at a fixture directory. Only
// ID (used as Finding.Project) and Path (joined onto the root to locate
// artifacts) matter to the doctor engine.
func proj(name string) *workspace.Project {
	return &workspace.Project{ID: "/" + name, Path: name}
}

// findingsByCode indexes findings by check code for assertions.
func findingsByCode(findings []doctor.Finding) map[doctor.CheckCode][]doctor.Finding {
	out := map[doctor.CheckCode][]doctor.Finding{}
	for _, f := range findings {
		out[f.Code] = append(out[f.Code], f)
	}
	return out
}

// TestDoctorRun_CleanIsEmpty asserts a project whose committed artifacts are
// valid and complete produces no findings and exits 0 under every profile.
func TestDoctorRun_CleanIsEmpty(t *testing.T) {
	for _, profile := range doctor.ProfileValues {
		report, err := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("clean")}, profile, doctorTestClock)
		if err != nil {
			t.Fatalf("profile %s: clean project returned error: %v", profile, err)
		}
		if len(report.Findings) != 0 {
			t.Fatalf("profile %s: clean project findings = %d, want 0: %#v", profile, len(report.Findings), report.Findings)
		}
		if report.Summary.Findings != 0 {
			t.Fatalf("profile %s: summary.Findings = %d, want 0", profile, report.Summary.Findings)
		}
		if report.ProtocolVersion != doctor.ProtocolVersion || report.Profile != profile {
			t.Fatalf("profile %s: report header = {v=%d profile=%s}", profile, report.ProtocolVersion, report.Profile)
		}
	}
}

// TestDoctorRun_ProductionBlocks asserts that under the production profile the
// missing-config and incomplete-capability projects yield blocking findings
// with the frozen check codes and profile-graded severities, and that the run
// returns an exit-2 error carrying the report.
func TestDoctorRun_ProductionBlocks(t *testing.T) {
	projects := []*workspace.Project{proj("missing-config"), proj("incomplete-capability")}
	report, err := DoctorRun(doctorTestdataRoot, projects, doctor.ProfileProduction, doctorTestClock)
	if err == nil {
		t.Fatal("production run with unsafe projects should return a blocking error")
	}
	if got := protocolcli.ExitCodeForError(err); got != protocolcli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", got, protocolcli.ExitUsage)
	}
	// The report must ride along on the error for the failure envelope.
	if data, ok := shared.ResultData(err).(doctor.Report); !ok || data.Summary.Findings != report.Summary.Findings {
		t.Fatalf("blocking error does not carry the report as ResultData: %#v", shared.ResultData(err))
	}

	byCode := findingsByCode(report.Findings)
	if n := len(byCode[doctor.CheckIncompleteCapability]); n != 2 {
		t.Fatalf("incomplete-capability findings = %d, want 2 (datasource, migration)", n)
	}
	missing := byCode[doctor.CheckMissingRequiredConfig]
	if len(missing) != 2 {
		t.Fatalf("missing-required-config findings = %d, want 2 (server.port, database.password)", len(missing))
	}

	// Severity grading: a missing sensitive value is critical, other unsafe
	// states are high, under production.
	var sawCritical, sawHigh bool
	for _, f := range missing {
		switch f.Evidence[0].Field {
		case "database.password":
			if f.Severity != doctor.SeverityCritical {
				t.Errorf("missing sensitive %s severity = %s, want critical", f.Evidence[0].Field, f.Severity)
			}
			sawCritical = true
		case "server.port":
			if f.Severity != doctor.SeverityHigh {
				t.Errorf("missing %s severity = %s, want high", f.Evidence[0].Field, f.Severity)
			}
			sawHigh = true
		default:
			t.Errorf("unexpected missing-config field %q", f.Evidence[0].Field)
		}
	}
	if !sawCritical || !sawHigh {
		t.Fatalf("expected both a critical and a high missing-config finding; critical=%v high=%v", sawCritical, sawHigh)
	}

	// Every finding stamps the baked remediation for its code.
	for _, f := range report.Findings {
		if f.Remediation != doctor.Remediation(f.Code) {
			t.Errorf("finding %s remediation = %q, want baked remediation", f.Code, f.Remediation)
		}
	}

	if report.Summary.Critical != 1 || report.Summary.High != 3 || report.Summary.Findings != 4 {
		t.Fatalf("summary = %#v, want 1 critical / 3 high / 4 total", report.Summary)
	}
}

// TestDoctorRun_DevPermissive asserts the same unsafe projects stay advisory
// (info) and exit 0 under the dev profile: only production promotes an unsafe
// state to a blocking severity.
func TestDoctorRun_DevPermissive(t *testing.T) {
	projects := []*workspace.Project{proj("missing-config"), proj("incomplete-capability")}
	report, err := DoctorRun(doctorTestdataRoot, projects, doctor.ProfileDev, doctorTestClock)
	if err != nil {
		t.Fatalf("dev run should not block: %v", err)
	}
	if report.Summary.Findings == 0 {
		t.Fatal("dev run should still surface findings, just at info severity")
	}
	for _, f := range report.Findings {
		if f.Severity != doctor.SeverityInfo {
			t.Errorf("dev finding %s severity = %s, want info", f.Code, f.Severity)
		}
	}
	if report.Summary.High != 0 || report.Summary.Critical != 0 {
		t.Fatalf("dev summary must have no blocking findings: %#v", report.Summary)
	}
}

// TestDoctorRun_Deterministic asserts the report is byte-stable across repeated
// runs over the same committed tree: no map iteration or check-order
// nondeterminism leaks into the findings or their ordering.
func TestDoctorRun_Deterministic(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "doctor-gate", "doctor-is-deterministic")
	projects := []*workspace.Project{
		proj("shadowing"), proj("missing-config"), proj("incomplete-capability"), proj("invalid-schema"), proj("clean"),
	}
	first, _ := DoctorRun(doctorTestdataRoot, projects, doctor.ProfileProduction, doctorTestClock)
	want, err := json.MarshalIndent(first, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := range 20 {
		// Shuffle the input order to prove ordering is content-derived, not
		// input-derived.
		shuffled := []*workspace.Project{
			proj("clean"), proj("invalid-schema"), proj("incomplete-capability"), proj("missing-config"), proj("shadowing"),
		}
		got, _ := DoctorRun(doctorTestdataRoot, shuffled, doctor.ProfileProduction, doctorTestClock)
		data, _ := json.MarshalIndent(got, "", "  ")
		if !bytes.Equal(data, want) {
			t.Fatalf("iteration %d not byte-stable:\n--- got:\n%s\n--- want:\n%s", i, data, want)
		}
	}
}

// TestDoctorRun_Redaction asserts a doctor report never carries a resolved
// config value: Evidence names a workspace-relative path and a field only, and
// the committed literal (8080) that triggered a shadowing finding never appears
// anywhere in the serialized report.
func TestDoctorRun_Redaction(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "redaction", "doctor-and-run-diagnostics-redact-secrets")
	projects := []*workspace.Project{proj("shadowing"), proj("missing-config")}
	report, _ := DoctorRun(doctorTestdataRoot, projects, doctor.ProfileProduction, doctorTestClock)

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(data, []byte("8080")) {
		t.Fatalf("serialized report leaked the resolved config value 8080:\n%s", data)
	}

	for _, f := range report.Findings {
		if len(f.Evidence) == 0 {
			t.Errorf("finding %s has no evidence", f.Code)
			continue
		}
		ev := f.Evidence[0]
		if ev.Path == "" {
			t.Errorf("finding %s evidence has empty path", f.Code)
		}
		if strings.HasPrefix(ev.Path, "/") || strings.Contains(ev.Path, doctorTestdataRoot) {
			t.Errorf("finding %s evidence path %q is not workspace-relative to the project", f.Code, ev.Path)
		}
	}
}

// TestDoctor_Shadowing asserts a field carrying both an env binding and a
// committed literal is a CheckConfigShadowing finding naming the field, not its
// value.
func TestDoctor_Shadowing(t *testing.T) {
	report, _ := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("shadowing")}, doctor.ProfileProduction, doctorTestClock)
	byCode := findingsByCode(report.Findings)
	shadow := byCode[doctor.CheckConfigShadowing]
	if len(shadow) != 1 {
		t.Fatalf("config-shadowing findings = %d, want 1: %#v", len(shadow), report.Findings)
	}
	f := shadow[0]
	if f.Evidence[0].Field != "server.port" {
		t.Errorf("shadowing field = %q, want server.port", f.Evidence[0].Field)
	}
	if f.Evidence[0].Path != "shadowing/schema/config.json" {
		t.Errorf("shadowing evidence path = %q, want shadowing/schema/config.json", f.Evidence[0].Path)
	}
	if strings.Contains(f.Message, "8080") {
		t.Errorf("shadowing message leaked the value: %q", f.Message)
	}
}

// TestDoctor_InvalidSchema asserts a committed config schema that fails its own
// validation is a single CheckInvalidGeneratedSchema finding.
func TestDoctor_InvalidSchema(t *testing.T) {
	report, _ := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("invalid-schema")}, doctor.ProfileProduction, doctorTestClock)
	byCode := findingsByCode(report.Findings)
	invalid := byCode[doctor.CheckInvalidGeneratedSchema]
	if len(invalid) != 1 {
		t.Fatalf("invalid-schema findings = %d, want 1: %#v", len(invalid), report.Findings)
	}
	if invalid[0].Evidence[0].Path != "invalid-schema/schema/config.json" {
		t.Errorf("invalid-schema evidence path = %q", invalid[0].Evidence[0].Path)
	}
}

// TestDoctor_BlankRequiredScalarStillMissing asserts a required scalar committed
// as a bare placeholder (`port:` with no value) still surfaces as missing —
// declaring the key is not the same as setting it. A required scalar that
// carries a value and a required object satisfied by a committed child must not
// be false positives.
func TestDoctor_BlankRequiredScalarStillMissing(t *testing.T) {
	report, err := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("blank-required")}, doctor.ProfileProduction, doctorTestClock)
	if err == nil {
		t.Fatal("a required scalar left as a bare placeholder must still block under production")
	}
	missing := findingsByCode(report.Findings)[doctor.CheckMissingRequiredConfig]
	fields := map[string]bool{}
	for _, f := range missing {
		fields[f.Evidence[0].Field] = true
	}
	if !fields["server.port"] {
		t.Errorf("bare `server.port:` placeholder must surface as missing; findings = %#v", missing)
	}
	if fields["server.host"] {
		t.Error("server.host carries a value and must not be flagged missing")
	}
	if fields["database.primary"] || fields["database.primary.url"] {
		t.Error("a required object satisfied by a committed child (and its assigned leaf) must not be flagged missing")
	}
	if len(missing) != 1 {
		t.Fatalf("want exactly one missing-required finding (server.port), got %d: %#v", len(missing), missing)
	}
}

// TestDoctor_SecurityDefaults asserts the four production-unsafe-marker checks
// fire under production: a marked field left unset by the committed production
// sources surfaces as the security code its config-path category warrants, with
// the taxonomy-graded severity (persistence and signing key are critical; the
// insecure transport and local rate limit are high), each naming the field path
// only and stamping the baked remediation.
func TestDoctor_SecurityDefaults(t *testing.T) {
	report, err := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("security-defaults")}, doctor.ProfileProduction, doctorTestClock)
	if err == nil {
		t.Fatal("production run with production-unsafe defaults should block")
	}
	byCode := findingsByCode(report.Findings)

	cases := []struct {
		code     doctor.CheckCode
		field    string
		severity doctor.Severity
	}{
		{doctor.CheckVolatilePersistence, "database.driver", doctor.SeverityCritical},
		{doctor.CheckEphemeralSigningKey, "auth.signingKey", doctor.SeverityCritical},
		{doctor.CheckInsecureTransport, "session.cookieSecure", doctor.SeverityHigh},
		{doctor.CheckLocalRateLimit, "rateLimit.store", doctor.SeverityHigh},
	}
	for _, c := range cases {
		got := byCode[c.code]
		if len(got) != 1 {
			t.Fatalf("%s findings = %d, want 1: %#v", c.code, len(got), report.Findings)
		}
		f := got[0]
		if f.Evidence[0].Field != c.field {
			t.Errorf("%s field = %q, want %q", c.code, f.Evidence[0].Field, c.field)
		}
		if f.Evidence[0].Path != "security-defaults/schema/config.json" {
			t.Errorf("%s evidence path = %q", c.code, f.Evidence[0].Path)
		}
		if f.Severity != c.severity {
			t.Errorf("%s severity = %s, want %s", c.code, f.Severity, c.severity)
		}
		if f.Remediation != doctor.Remediation(c.code) {
			t.Errorf("%s remediation = %q, want baked", c.code, f.Remediation)
		}
	}
	if report.Summary.Findings != 4 || report.Summary.Critical != 2 || report.Summary.High != 2 {
		t.Fatalf("summary = %#v, want 4 total / 2 critical / 2 high", report.Summary)
	}
}

// TestDoctor_SecurityDefaultsSetIsClean asserts a marked field the production
// sources bind to a concrete value never falls back to its unsafe default, so it
// produces no security finding — the "unset in production sources" gate.
func TestDoctor_SecurityDefaultsSetIsClean(t *testing.T) {
	report, err := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("security-defaults-set")}, doctor.ProfileProduction, doctorTestClock)
	if err != nil {
		t.Fatalf("all production-unsafe defaults are bound, so the run must not block: %v", err)
	}
	for _, f := range report.Findings {
		switch f.Code {
		case doctor.CheckVolatilePersistence, doctor.CheckEphemeralSigningKey,
			doctor.CheckInsecureTransport, doctor.CheckLocalRateLimit:
			t.Errorf("bound field still produced a security finding: %s %s", f.Code, f.Evidence[0].Field)
		}
	}
}

// TestDoctor_SecurityDefaultsDevPermissive asserts the marker checks stay
// advisory (info) and exit 0 under dev: only production promotes them to a
// blocking severity.
func TestDoctor_SecurityDefaultsDevPermissive(t *testing.T) {
	report, err := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("security-defaults")}, doctor.ProfileDev, doctorTestClock)
	if err != nil {
		t.Fatalf("dev run must not block: %v", err)
	}
	if report.Summary.Findings != 4 {
		t.Fatalf("dev summary findings = %d, want 4", report.Summary.Findings)
	}
	for _, f := range report.Findings {
		if f.Severity != doctor.SeverityInfo {
			t.Errorf("dev finding %s severity = %s, want info", f.Code, f.Severity)
		}
	}
}

// TestDoctor_SecurityRedaction asserts a security finding never leaks a resolved
// value: Evidence names the schema path and field only.
func TestDoctor_SecurityRedaction(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "redaction", "doctor-and-run-diagnostics-redact-secrets")
	report, _ := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("security-defaults")}, doctor.ProfileProduction, doctorTestClock)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The schema defaults (memory, false) are field defaults, not resolved
	// values — but a signing key value must never appear. There is none in the
	// schema, and none is read; assert the marker field carries a field name only.
	for _, f := range report.Findings {
		if len(f.Evidence) != 1 || f.Evidence[0].Field == "" {
			t.Errorf("security finding %s must carry exactly one field-named evidence: %#v", f.Code, f.Evidence)
		}
	}
	if bytes.Contains(data, []byte("kms://")) {
		t.Fatalf("report leaked a config value:\n%s", data)
	}
}

// TestClassifyUnsafeDefault pins the config-path category classifier the
// security checks route through: signing-key, rate-limit, transport, and
// persistence keywords each resolve to their frozen code in a fixed priority
// order, and an uncategorizable marked field is skipped (no guess).
func TestClassifyUnsafeDefault(t *testing.T) {
	tests := []struct {
		path string
		code doctor.CheckCode
		ok   bool
	}{
		{"auth.signingKey", doctor.CheckEphemeralSigningKey, true},
		{"security.keyring.active", doctor.CheckEphemeralSigningKey, true},
		{"rateLimit.store", doctor.CheckLocalRateLimit, true}, // ratelimit wins over store
		{"session.cookieSecure", doctor.CheckInsecureTransport, true},
		{"server.transport", doctor.CheckInsecureTransport, true},
		{"database.driver", doctor.CheckVolatilePersistence, true},
		{"cache.store", doctor.CheckVolatilePersistence, true},
		{"server.port", "", false},
	}
	for _, tt := range tests {
		code, ok := classifyUnsafeDefault(tt.path)
		if ok != tt.ok || code != tt.code {
			t.Errorf("classifyUnsafeDefault(%q) = (%q, %v), want (%q, %v)", tt.path, code, ok, tt.code, tt.ok)
		}
	}
}

// TestDoctorJSONLEnvelope asserts the structured envelope `putnami doctor`
// ACTUALLY emits, for both a clean (success) and a blocking (failure) run.
//
// The success half drives the command's own renderer (renderDoctor), not a
// document rebuilt beside it: the version-1 spelling this test used to assert
// was retired by B1b, and because it never called the production path
// the retirement moved nothing here. The failure half drives the constructor
// the dispatcher uses, because renderDoctor deliberately writes NOTHING then —
// the report travels on the classified error and internal/cli's
// writeStructuredFailure turns it into the envelope — and that silence is
// asserted rather than assumed.
func TestDoctorJSONLEnvelope(t *testing.T) {
	// The report rides in data, so the envelope is decoded with data TYPED as a
	// doctor.Report; protocolVersion is asserted against the contract's own
	// constant, since a document without it is a version-1 document.
	type envelope struct {
		ProtocolVersion int           `json:"protocolVersion"`
		Command         string        `json:"command"`
		Status          string        `json:"status"`
		ExitCode        int           `json:"exitCode"`
		Data            doctor.Report `json:"data"`
		Error           *struct {
			Code string `json:"code"`
		} `json:"error"`
	}

	// Success: a clean project emits a success envelope carrying the report.
	cleanReport, cleanErr := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("clean")}, doctor.ProfileProduction, doctorTestClock)
	if cleanErr != nil {
		t.Fatalf("clean project reported a blocking error: %v", cleanErr)
	}
	var buf bytes.Buffer
	if err := renderDoctor(&buf, "jsonl", cleanReport, cleanErr); err != nil {
		t.Fatalf("renderDoctor(clean) = %v, want nil", err)
	}
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("structured output must be a single JSONL line, got:\n%s", buf.String())
	}
	var ok envelope
	if err := json.Unmarshal(buf.Bytes(), &ok); err != nil {
		t.Fatalf("unmarshal success envelope: %v", err)
	}
	if ok.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("success envelope protocolVersion = %d, want %d — an envelope without it is version 1",
			ok.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	if ok.Command != "doctor" || ok.Status != protocolcli.StatusSuccess || ok.ExitCode != 0 {
		t.Fatalf("success envelope = %+v", ok)
	}
	if ok.Data.ProtocolVersion != doctor.ProtocolVersion {
		t.Fatalf("success envelope data missing report: %+v", ok.Data)
	}

	// Failure: renderDoctor emits nothing and returns the blocking error
	// unchanged, so the dispatcher can classify it.
	failReport, failErr := DoctorRun(doctorTestdataRoot, []*workspace.Project{proj("missing-config")}, doctor.ProfileProduction, doctorTestClock)
	if failErr == nil {
		t.Fatal("expected a blocking error for the failure envelope")
	}
	buf.Reset()
	if err := renderDoctor(&buf, "jsonl", failReport, failErr); !errors.Is(err, failErr) {
		t.Fatalf("renderDoctor(blocking) = %v, want the run error unchanged", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("renderDoctor wrote a document on a blocking run; the dispatcher owns that envelope:\n%s",
			buf.String())
	}

	// The dispatcher's envelope: exit 2, the "usage" class, and the report
	// attached as data via ResultData on the classified error.
	buf.Reset()
	code, err := protocolcli.WriteResultV2(&buf, protocolcli.OutputJSONL,
		protocolcli.NewResultV2("doctor", shared.ResultData(failErr), failErr))
	if err != nil {
		t.Fatalf("write failure result: %v", err)
	}
	if code != protocolcli.ExitUsage {
		t.Errorf("failure exit code = %d, want %d", code, protocolcli.ExitUsage)
	}
	var bad envelope
	if err := json.Unmarshal(buf.Bytes(), &bad); err != nil {
		t.Fatalf("unmarshal failure envelope: %v", err)
	}
	if bad.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("failure envelope protocolVersion = %d, want %d",
			bad.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	if bad.Status != protocolcli.StatusFailure || bad.ExitCode != protocolcli.ExitUsage {
		t.Fatalf("failure envelope = %+v", bad)
	}
	if bad.Error == nil || bad.Error.Code != "usage" {
		t.Fatalf("failure envelope error = %+v, want code usage", bad.Error)
	}
	if bad.Data.Summary.Findings != failReport.Summary.Findings {
		t.Fatalf("failure envelope data findings = %d, want %d", bad.Data.Summary.Findings, failReport.Summary.Findings)
	}
}

// TestSeverityFor pins the profile-graded severity model the whole command
// keys off: production is strict (high, critical for a missing sensitive value)
// while dev/test stay permissive.
func TestSeverityFor(t *testing.T) {
	tests := []struct {
		code      doctor.CheckCode
		profile   doctor.Profile
		sensitive bool
		want      doctor.Severity
	}{
		{doctor.CheckIncompleteCapability, doctor.ProfileProduction, false, doctor.SeverityHigh},
		{doctor.CheckMissingRequiredConfig, doctor.ProfileProduction, false, doctor.SeverityHigh},
		{doctor.CheckMissingRequiredConfig, doctor.ProfileProduction, true, doctor.SeverityCritical},
		{doctor.CheckConfigShadowing, doctor.ProfileProduction, false, doctor.SeverityHigh},
		{doctor.CheckMissingRequiredConfig, doctor.ProfileTest, true, doctor.SeverityWarning},
		{doctor.CheckMissingRequiredConfig, doctor.ProfileDev, true, doctor.SeverityInfo},
	}
	for _, tt := range tests {
		if got := severityFor(tt.code, tt.profile, tt.sensitive); got != tt.want {
			t.Errorf("severityFor(%s, %s, sensitive=%v) = %s, want %s", tt.code, tt.profile, tt.sensitive, got, tt.want)
		}
	}
}

// TestYAMLKeyPaths exercises the minimal block-map extractor: nesting by
// indentation, empty placeholders (declared but not assigned), quoted keys,
// comment-only values, values that contain colons, and skipped sequence entries
// — and it must never expose a value.
func TestYAMLKeyPaths(t *testing.T) {
	src := []byte(`# comment
app:
  name: "Config Sample"
  debug: false
database:
  primary:
    host: db.internal
    password: ""
    url: "https://db.internal:5432/app"
  placeholder:
  onlyComment: # nothing here
tags:
  - one
  - two
"quoted": value
`)
	declared, assigned := yamlKeyPaths(src)

	wantDeclared := []string{
		"app", "app.name", "app.debug",
		"database", "database.primary", "database.primary.host",
		"database.primary.password", "database.primary.url",
		"database.placeholder", "database.onlyComment",
		"tags", "quoted",
	}
	for _, k := range wantDeclared {
		if !declared[k] {
			t.Errorf("declared missing %q", k)
		}
	}

	// Value-carrying leaves are assigned; map parents and empty/comment-only
	// leaves are not.
	for _, k := range []string{"app.name", "app.debug", "database.primary.host", "database.primary.password", "database.primary.url", "quoted"} {
		if !assigned[k] {
			t.Errorf("assigned missing %q", k)
		}
	}
	for _, k := range []string{"app", "database", "database.primary", "database.placeholder", "database.onlyComment"} {
		if assigned[k] {
			t.Errorf("map parent / empty leaf %q must not be assigned", k)
		}
	}
	// The colon inside the URL value must not create a spurious key.
	if declared["database.primary.url.5432/app"] || declared["https"] {
		t.Error("a colon inside a value was misparsed as a key path")
	}
}
