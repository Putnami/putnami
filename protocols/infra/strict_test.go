package infra

import (
	"strings"
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

func TestParsePerProjectManifest_Valid(t *testing.T) {
	data := []byte(`{"protocolVersion": 2, "secrets": ["a"]}`)
	m, diags := ParsePerProjectManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil {
		t.Fatal("manifest should not be nil")
	}
	if len(m.Secrets) != 1 || m.Secrets[0] != "a" {
		t.Errorf("Secrets = %v, want [a]", m.Secrets)
	}
}

func TestParsePerProjectManifest_UnknownField(t *testing.T) {
	data := []byte(`{"protocolVersion": 2, "unknownField": true}`)
	m, diags := ParsePerProjectManifest(data)
	if m != nil {
		t.Error("manifest should be nil on parse error")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParsePerProjectManifest_RuntimeInLibrary(t *testing.T) {
	data := []byte(`{"protocolVersion": 2, "runtime": {"ingress": {"domain": "x"}}}`)
	_, diags := ParsePerProjectManifest(data)
	if !findCode(diags, ErrorCodeRuntimeInLibrary) {
		t.Errorf("want %s, got %v", ErrorCodeRuntimeInLibrary, diags)
	}
}

func TestParsePerProjectManifest_InvalidJSON(t *testing.T) {
	_, diags := ParsePerProjectManifest([]byte("{not json"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
	if diags[0].Code != ErrorCodeParseError {
		t.Errorf("Code = %q, want %s", diags[0].Code, ErrorCodeParseError)
	}
}

func TestValidatePerProjectManifest_InvalidName(t *testing.T) {
	m := &PerProjectManifest{
		ProtocolVersion: ProtocolVersion,
		Databases:       []Database{{Name: "Bad!Name", Engine: EnginePostgres}},
	}
	diags := ValidatePerProjectManifest(m)
	if !findCode(diags, ErrorCodeInvalidName) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidName, diags)
	}
}

func TestValidatePerProjectManifest_InvalidEngine(t *testing.T) {
	m := &PerProjectManifest{
		ProtocolVersion: ProtocolVersion,
		Databases:       []Database{{Name: "primary", Engine: "mongo"}},
	}
	diags := ValidatePerProjectManifest(m)
	if !findCode(diags, ErrorCodeInvalidEngine) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidEngine, diags)
	}
}

func TestValidatePerProjectManifest_InvalidProtocolVersion(t *testing.T) {
	// 99 is not the supported protocol version, so the parser rejects it.
	m := &PerProjectManifest{ProtocolVersion: 99}
	diags := ValidatePerProjectManifest(m)
	if !findCode(diags, ErrorCodeInvalidProtocolVersion) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidProtocolVersion, diags)
	}
}

// TestValidatePerProjectManifest_PreviousProtocolVersionIsRejected pins the
// break this protocol version exists to make. Every manifest written before
// the v2 bump is stamped 1, and a reader that quietly widened back to
// "1 or 2" would let a workload keep declaring a fixed-cost floor while every
// other test stayed green — the 99 case above cannot catch that, because 99
// is rejected either way.
func TestValidatePerProjectManifest_PreviousProtocolVersionIsRejected(t *testing.T) {
	for v := 1; v < ProtocolVersion; v++ {
		m := &PerProjectManifest{ProtocolVersion: v}
		diags := ValidatePerProjectManifest(m)
		if !findCode(diags, ErrorCodeInvalidProtocolVersion) {
			t.Fatalf("protocolVersion %d: want %s, got %v", v, ErrorCodeInvalidProtocolVersion, diags)
		}
		// The reader cannot repair the file, so the diagnostic must say who can.
		if !strings.Contains(diags[0].Message, MigrationGuide) {
			t.Errorf("protocolVersion %d: message %q does not carry the migration guide", v, diags[0].Message)
		}
	}
}

// TestParseAggregatedManifest_RemovedCostPolicyFieldNamesTheMigration proves a
// retired knob is rejected with an explanation. encoding/json would say only
// `unknown field "min"`, which reads as a typo rather than as a deliberate
// ownership change — and the aggregator turns this into a warning, so the
// message is the author's only signal.
func TestParseAggregatedManifest_RemovedCostPolicyFieldNamesTheMigration(t *testing.T) {
	for field, body := range map[string]string{
		"min":     `{"protocolVersion": 2, "workload": "w", "runtime": {"scaling": {"min": 1}}}`,
		"billing": `{"protocolVersion": 2, "workload": "w", "runtime": {"billing": "instance-based"}}`,
	} {
		t.Run(field, func(t *testing.T) {
			_, diags := ParseAggregatedManifest([]byte(body))
			if !findCode(diags, ErrorCodeUnknownField) {
				t.Fatalf("want %s, got %v", ErrorCodeUnknownField, diags)
			}
			msg := diags[0].Message
			for _, want := range []string{field, RemovedRuntimeFields[field], MigrationGuide} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not mention %q", msg, want)
				}
			}
		})
	}
}

func TestValidatePerProjectManifest_EmptySchedule(t *testing.T) {
	m := &PerProjectManifest{
		ProtocolVersion: ProtocolVersion,
		ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: ""}},
	}
	diags := ValidatePerProjectManifest(m)
	if !findCode(diags, ErrorCodeEmptySchedule) {
		t.Errorf("want %s, got %v", ErrorCodeEmptySchedule, diags)
	}
}

func TestValidatePerProjectManifest_InvalidSchedule(t *testing.T) {
	// Non-empty schedules that are not well-formed 5-field cron strings.
	cases := []struct {
		name     string
		schedule string
	}{
		{"extra-field", "0 0 1 1 1 1 1"},
		{"macro", "@every 5min"},
		{"minute-out-of-range", "60 0 * * *"},
		{"unknown-weekday", "0 0 * * funday"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &PerProjectManifest{
				ProtocolVersion: ProtocolVersion,
				ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: tc.schedule}},
			}
			diags := ValidatePerProjectManifest(m)
			if !findCode(diags, ErrorCodeInvalidSchedule) {
				t.Errorf("want %s for schedule %q, got %v", ErrorCodeInvalidSchedule, tc.schedule, diags)
			}
		})
	}
}

func TestValidatePerProjectManifest_WhitespaceScheduleIsEmptyNotInvalid(t *testing.T) {
	// A whitespace-only schedule is reported as empty, never as invalid —
	// validateScheduledJob returns after the empty check so the two codes
	// don't both fire for the same value.
	m := &PerProjectManifest{
		ProtocolVersion: ProtocolVersion,
		ScheduledJobs:   []ScheduledJob{{Name: "rollup", Schedule: "   "}},
	}
	diags := ValidatePerProjectManifest(m)
	if !findCode(diags, ErrorCodeEmptySchedule) {
		t.Errorf("want %s for whitespace-only schedule, got %v", ErrorCodeEmptySchedule, diags)
	}
	if findCode(diags, ErrorCodeInvalidSchedule) {
		t.Errorf("empty schedule must not also report %s: %v", ErrorCodeInvalidSchedule, diags)
	}
}

func TestValidateCronExpression(t *testing.T) {
	valid := []string{
		"* * * * *",
		"0 2 * * *",
		"0 0 1 * *",
		"*/15 * * * *",
		"0 9-17 * * 1-5",
		"0 0 * * MON-FRI",
		"0 0 1,15 * *",
		"0 0 1 JAN,JUL *",
		"0,30 0/6 * * SUN",
		"0 0 * * 7",      // 7 is Sunday, same as 0
		"*/59 * * * *",   // step equal to the field maximum is allowed
		"0   2  *  *  *", // runs of whitespace collapse
	}
	for _, expr := range valid {
		if err := validateCronExpression(expr); err != nil {
			t.Errorf("validateCronExpression(%q) = %v, want nil", expr, err)
		}
	}

	invalid := []string{
		"",               // no fields
		"* * * *",        // 4 fields
		"* * * * * *",    // 6 fields (no seconds field in Cloud Scheduler)
		"0 0 1 1 1 1 1",  // 7 fields
		"@daily",         // macro
		"@every 5min",    // macro
		"60 * * * *",     // minute out of range
		"* 24 * * *",     // hour out of range
		"* * 0 * *",      // day-of-month below 1
		"* * 32 * *",     // day-of-month above 31
		"* * * 13 *",     // month out of range
		"* * * * 8",      // day-of-week above 7
		"*/0 * * * *",    // zero step
		"*/60 * * * *",   // step exceeds minute maximum
		"5-1 * * * *",    // descending range
		"1,,3 * * * *",   // empty list element
		"* * * * funday", // unknown weekday name
		"L * * * *",      // Quartz token
	}
	for _, expr := range invalid {
		if err := validateCronExpression(expr); err == nil {
			t.Errorf("validateCronExpression(%q) = nil, want error", expr)
		}
	}
}

func TestValidateAggregatedManifest_MissingWorkload(t *testing.T) {
	m := &AggregatedManifest{ProtocolVersion: ProtocolVersion}
	diags := ValidateAggregatedManifest(m)
	if !findCode(diags, ErrorCodeMissingWorkload) {
		t.Errorf("want %s, got %v", ErrorCodeMissingWorkload, diags)
	}
}

func TestValidateAggregatedManifest_InvalidContributor(t *testing.T) {
	m := &AggregatedManifest{
		ProtocolVersion: ProtocolVersion,
		Workload:        "w",
		Secrets: []AggregatedSecret{{
			Name:    "a",
			Sources: []Source{{Project: "p", Contributor: "bogus"}},
		}},
	}
	diags := ValidateAggregatedManifest(m)
	if !findCode(diags, ErrorCodeInvalidContributor) {
		t.Errorf("want %s, got %v", ErrorCodeInvalidContributor, diags)
	}
}

func TestValidateAggregatedManifest_EmptyProjectInSource(t *testing.T) {
	m := &AggregatedManifest{
		ProtocolVersion: ProtocolVersion,
		Workload:        "w",
		Secrets: []AggregatedSecret{{
			Name:    "a",
			Sources: []Source{{Project: "", Contributor: ContributorManual}},
		}},
	}
	diags := ValidateAggregatedManifest(m)
	if !findCode(diags, ErrorCodeInvalidContributor) {
		t.Errorf("expected invalid contributor diagnostic for empty project, got %v", diags)
	}
}

func TestValidateAggregatedManifest_MissingSources(t *testing.T) {
	m := &AggregatedManifest{
		ProtocolVersion: ProtocolVersion,
		Workload:        "w",
		Databases: []AggregatedDatabase{{
			Name:    "primary",
			Engine:  EnginePostgres,
			Sources: nil,
		}},
	}
	diags := ValidateAggregatedManifest(m)
	if !findCode(diags, ErrorCodeMissingSources) {
		t.Errorf("want %s for nil Sources, got %v", ErrorCodeMissingSources, diags)
	}

	m.Databases[0].Sources = []Source{}
	diags = ValidateAggregatedManifest(m)
	if !findCode(diags, ErrorCodeMissingSources) {
		t.Errorf("want %s for empty Sources slice, got %v", ErrorCodeMissingSources, diags)
	}
}

func TestValidateAggregatedManifest_RuntimeScalingNegative(t *testing.T) {
	neg := -1
	cases := []struct {
		name    string
		scaling Scaling
	}{
		{"max", Scaling{Max: &neg}},
		{"concurrency", Scaling{Concurrency: &neg}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &AggregatedManifest{
				ProtocolVersion: ProtocolVersion,
				Workload:        "w",
				Runtime:         &Runtime{Scaling: &tc.scaling},
			}
			diags := ValidateAggregatedManifest(m)
			if !findCode(diags, ErrorCodeInvalidScaling) {
				t.Errorf("want %s for negative scaling.%s, got %v", ErrorCodeInvalidScaling, tc.name, diags)
			}
		})
	}
}

func TestValidateAggregatedManifest_RuntimeScalingZeroIsValid(t *testing.T) {
	zero := 0
	m := &AggregatedManifest{
		ProtocolVersion: ProtocolVersion,
		Workload:        "w",
		Runtime: &Runtime{
			Scaling: &Scaling{Max: &zero, Concurrency: &zero},
		},
	}
	diags := ValidateAggregatedManifest(m)
	if findCode(diags, ErrorCodeInvalidScaling) {
		t.Errorf("zero scaling values are valid; got %v", diags)
	}
}

func TestParsePerProjectManifest_NestedRuntimeNotMisclassified(t *testing.T) {
	// A nested object containing a "runtime" key inside an unknown
	// top-level field must NOT be reported as runtime_in_library — it
	// is a generic unknown_field rejection on the outer key. This guards
	// against a regex-based sniff that would scan the whole payload.
	data := []byte(`{
		"protocolVersion": 2,
		"unknownTopLevel": { "runtime": "value" }
	}`)
	_, diags := ParsePerProjectManifest(data)
	if findCode(diags, ErrorCodeRuntimeInLibrary) {
		t.Errorf("nested runtime key was misclassified as runtime_in_library: %v", diags)
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s for unknown top-level field, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestValidateContributor(t *testing.T) {
	cases := []struct {
		c    ContributorID
		want bool
	}{
		{ContributorManual, true},
		{FrameworkContributor("go.putnami.dev/database"), true},
		{"framework:", false},
		{"manual ", false},
		{"random", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.c), func(t *testing.T) {
			diags := ValidateContributor("contributor", tc.c)
			gotOK := !diag.HasErrors(diags)
			if gotOK != tc.want {
				t.Errorf("ValidateContributor(%q) ok=%v, want %v (diags=%v)", tc.c, gotOK, tc.want, diags)
			}
		})
	}
}

func TestParseAggregatedManifest_UnknownField(t *testing.T) {
	data := []byte(`{"protocolVersion": 2, "workload": "w", "extra": true}`)
	_, diags := ParseAggregatedManifest(data)
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParseAggregatedManifest_SecurityEventsGateway(t *testing.T) {
	data := []byte(`{
		"protocolVersion": 2,
		"workload": "w",
		"runtime": {"security": {"eventsGateway": true}}
	}`)
	m, diags := ParseAggregatedManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil || m.Runtime == nil || m.Runtime.Security == nil ||
		m.Runtime.Security.EventsGateway == nil || !*m.Runtime.Security.EventsGateway {
		t.Fatalf("EventsGateway = %v, want true", m)
	}

	data = []byte(`{
		"protocolVersion": 2,
		"workload": "w",
		"runtime": {"security": {"unknownSecurityKey": true}}
	}`)
	m, diags = ParseAggregatedManifest(data)
	if m != nil {
		t.Errorf("manifest should be nil on nested unknown-field parse error")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s for unknown security key, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestValidErrorCodes_Membership(t *testing.T) {
	required := []string{
		ErrorCodeParseError,
		ErrorCodeUnknownField,
		ErrorCodeInvalidName,
		ErrorCodeInvalidEngine,
		ErrorCodeInvalidProtocolVersion,
		ErrorCodeInvalidContributor,
		ErrorCodeRuntimeInLibrary,
		ErrorCodeMissingWorkload,
		ErrorCodeMissingSources,
		ErrorCodeInvalidScaling,
		ErrorCodeEmptySchedule,
		ErrorCodeInvalidSchedule,
		ErrorCodeConflictingValue,
		ErrorCodeNonCanonical,
	}
	for _, code := range required {
		if !ValidErrorCodes[code] {
			t.Errorf("ValidErrorCodes missing %q", code)
		}
	}
}
