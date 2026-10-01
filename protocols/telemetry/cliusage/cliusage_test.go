package cliusage

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	telemetry "go.putnami.dev/protocol/telemetry"
)

func recordFromAttrs(attrs map[string]telemetry.AnyValue) telemetry.LogRecord {
	kvs := make([]telemetry.KeyValue, 0, len(attrs))
	for key, value := range attrs {
		kvs = append(kvs, telemetry.Attr(key, value))
	}
	return telemetry.LogRecord{Attributes: telemetry.SortAttrs(kvs)}
}

func startAttrs() map[string]telemetry.AnyValue {
	return map[string]telemetry.AnyValue{
		AttrEventName:    telemetry.StringVal(EventSessionStart),
		AttrDeviceID:     telemetry.StringVal("device-123"),
		AttrCLIVersion:   telemetry.StringVal("v1.2.3"),
		AttrOS:           telemetry.StringVal("darwin"),
		AttrArch:         telemetry.StringVal("arm64"),
		AttrCommands:     telemetry.StringVal("build,test"),
		AttrProjects:     telemetry.IntVal(2),
		AttrJobs:         telemetry.IntVal(1),
		AttrInteractive:  telemetry.BoolVal(true),
		AttrFlagImpacted: telemetry.BoolVal(false),
		AttrFlagCoverage: telemetry.BoolVal(false),
		AttrFlagOutput:   telemetry.BoolVal(false),
		AttrFlagNoCache:  telemetry.BoolVal(false),
		AttrFlagProjects: telemetry.BoolVal(false),
		AttrFlagWatch:    telemetry.BoolVal(false),
	}
}

func endAttrs() map[string]telemetry.AnyValue {
	return map[string]telemetry.AnyValue{
		AttrEventName:   telemetry.StringVal(EventSessionEnd),
		AttrDeviceID:    telemetry.StringVal("device-123"),
		AttrCLIVersion:  telemetry.StringVal("v1.2.3"),
		AttrOS:          telemetry.StringVal("darwin"),
		AttrArch:        telemetry.StringVal("arm64"),
		AttrSuccess:     telemetry.BoolVal(true),
		AttrDuration:    telemetry.IntVal(42),
		AttrInteractive: telemetry.BoolVal(false),
	}
}

func TestValidateRecordAcceptsConformingEvents(t *testing.T) {
	failure := endAttrs()
	failure[AttrSuccess] = telemetry.BoolVal(false)
	failure[AttrErrorCategory] = telemetry.StringVal(ErrorCategoryFailure)

	for name, attrs := range map[string]map[string]telemetry.AnyValue{
		"session:start":       startAttrs(),
		"session:end success": endAttrs(),
		"session:end failure": failure,
	} {
		t.Run(name, func(t *testing.T) {
			if diags := ValidateRecord(recordFromAttrs(attrs)); diag.HasErrors(diags) {
				t.Fatalf("conforming %s rejected: %v", name, diags)
			}
		})
	}
}

func TestValidateRecordRejectsViolations(t *testing.T) {
	tests := []struct {
		name string
		want string // expected error code
		mut  func(map[string]telemetry.AnyValue)
	}{
		{
			name: "unknown event",
			want: ErrorCodeUnknownEvent,
			mut:  func(a map[string]telemetry.AnyValue) { a[AttrEventName] = telemetry.StringVal("session:middle") },
		},
		{
			name: "missing event name",
			want: ErrorCodeUnknownEvent,
			mut:  func(a map[string]telemetry.AnyValue) { delete(a, AttrEventName) },
		},
		{
			name: "unknown attribute",
			want: ErrorCodeUnknownAttribute,
			mut:  func(a map[string]telemetry.AnyValue) { a["path"] = telemetry.StringVal("/private/workspace") },
		},
		{
			name: "wrong value kind",
			want: ErrorCodeAttributeKind,
			mut:  func(a map[string]telemetry.AnyValue) { a[AttrProjects] = telemetry.StringVal("two") },
		},
		{
			name: "missing required attribute",
			want: ErrorCodeMissingAttribute,
			mut:  func(a map[string]telemetry.AnyValue) { delete(a, AttrJobs) },
		},
		{
			name: "missing envelope attribute",
			want: ErrorCodeMissingAttribute,
			mut:  func(a map[string]telemetry.AnyValue) { delete(a, AttrDeviceID) },
		},
		{
			name: "free-text device id",
			want: ErrorCodeInvalidValue,
			mut:  func(a map[string]telemetry.AnyValue) { a[AttrDeviceID] = telemetry.StringVal("customer@example.com") },
		},
		{
			name: "free-text cli version",
			want: ErrorCodeInvalidValue,
			mut: func(a map[string]telemetry.AnyValue) {
				a[AttrCLIVersion] = telemetry.StringVal("local build from /tmp")
			},
		},
		{
			name: "unknown os",
			want: ErrorCodeInvalidValue,
			mut:  func(a map[string]telemetry.AnyValue) { a[AttrOS] = telemetry.StringVal("my-laptop") },
		},
		{
			name: "unknown architecture",
			want: ErrorCodeInvalidValue,
			mut:  func(a map[string]telemetry.AnyValue) { a[AttrArch] = telemetry.StringVal("fast-cpu") },
		},
		{
			name: "command path injection",
			want: ErrorCodeInvalidValue,
			mut:  func(a map[string]telemetry.AnyValue) { a[AttrCommands] = telemetry.StringVal("build,/private/repo") },
		},
		{
			name: "syntactically valid unknown command",
			want: ErrorCodeInvalidValue,
			mut:  func(a map[string]telemetry.AnyValue) { a[AttrCommands] = telemetry.StringVal("attacker-command") },
		},
		{
			name: "too many commands",
			want: ErrorCodeInvalidValue,
			mut: func(a map[string]telemetry.AnyValue) {
				a[AttrCommands] = telemetry.StringVal("a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := startAttrs()
			tt.mut(attrs)
			assertHasCode(t, ValidateRecord(recordFromAttrs(attrs)), tt.want)
		})
	}
}

func TestValidateRecordRejectsSessionEndViolations(t *testing.T) {
	tests := []struct {
		name string
		want string
		mut  func(map[string]telemetry.AnyValue)
	}{
		{
			name: "unknown error category",
			want: ErrorCodeUnknownErrorCategory,
			mut: func(a map[string]telemetry.AnyValue) {
				a[AttrSuccess] = telemetry.BoolVal(false)
				a[AttrErrorCategory] = telemetry.StringVal("network timeout")
			},
		},
		{
			name: "successful session with error category",
			want: ErrorCodeSuccessConsistency,
			mut: func(a map[string]telemetry.AnyValue) {
				a[AttrErrorCategory] = telemetry.StringVal(ErrorCategoryFailure)
			},
		},
		{
			name: "failed session without error category",
			want: ErrorCodeSuccessConsistency,
			mut: func(a map[string]telemetry.AnyValue) {
				a[AttrSuccess] = telemetry.BoolVal(false)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := endAttrs()
			tt.mut(attrs)
			assertHasCode(t, ValidateRecord(recordFromAttrs(attrs)), tt.want)
		})
	}
}

func TestValidateLogsRejectsForeignService(t *testing.T) {
	req := telemetry.LogsRequest{ResourceLogs: []telemetry.ResourceLogs{{
		Resource: telemetry.ResourceFromStrings(map[string]string{telemetry.AttrServiceName: "other-service"}),
		ScopeLogs: []telemetry.ScopeLogs{{
			LogRecords: []telemetry.LogRecord{recordFromAttrs(startAttrs())},
		}},
	}}}
	assertHasCode(t, ValidateLogs(req), ErrorCodeInvalidService)
}

func TestRequiredAttributesAreAllowed(t *testing.T) {
	for event, required := range RequiredAttributes {
		allowed := AllowedAttributes[event]
		if allowed == nil {
			t.Fatalf("event %q has required attributes but no AllowedAttributes entry", event)
		}
		for key := range required {
			if _, ok := allowed[key]; !ok {
				t.Errorf("%s requires %q but it is absent from AllowedAttributes", event, key)
			}
		}
	}
}

func assertHasCode(t *testing.T, diags []diag.Diagnostic, code string) {
	t.Helper()
	if !diag.HasErrors(diags) {
		t.Fatalf("expected an error diagnostic with code %q, got none", code)
	}
	for _, d := range diags {
		if d.Code == code {
			return
		}
	}
	t.Fatalf("diagnostics %v do not include expected code %q", diags, code)
}
