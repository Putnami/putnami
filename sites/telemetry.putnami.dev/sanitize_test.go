package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"
)

// --- builders -------------------------------------------------------------

func cliResource(extra ...telemetry.KeyValue) telemetry.Resource {
	attrs := append([]telemetry.KeyValue{
		telemetry.Attr(telemetry.AttrServiceName, telemetry.StringVal(cliusage.ServiceName)),
		telemetry.Attr(telemetry.AttrServiceVersion, telemetry.StringVal("v1.2.3")),
		telemetry.Attr(telemetry.AttrPutnamiFramework, telemetry.StringVal(telemetry.FrameworkGo)),
	}, extra...)
	return telemetry.Resource{Attributes: attrs}
}

func logsReq(res telemetry.Resource, records ...telemetry.LogRecord) *telemetry.LogsRequest {
	return &telemetry.LogsRequest{ResourceLogs: []telemetry.ResourceLogs{{
		Resource: res,
		ScopeLogs: []telemetry.ScopeLogs{{
			Scope:      telemetry.Scope{Name: "putnami-cli", Version: "v1.2.3"},
			LogRecords: records,
		}},
	}}}
}

// validStartRecord is a fully conformant session:start record.
func validStartRecord() telemetry.LogRecord {
	return telemetry.LogRecord{
		SeverityNumber: telemetry.SeverityInfo,
		SeverityText:   "info",
		Attributes: []telemetry.KeyValue{
			telemetry.Attr(cliusage.AttrEventName, telemetry.StringVal(cliusage.EventSessionStart)),
			telemetry.Attr(cliusage.AttrDeviceID, telemetry.StringVal("0123456789abcdef0123456789abcdef")),
			telemetry.Attr(cliusage.AttrCLIVersion, telemetry.StringVal("v1.2.3")),
			telemetry.Attr(cliusage.AttrOS, telemetry.StringVal("darwin")),
			telemetry.Attr(cliusage.AttrArch, telemetry.StringVal("arm64")),
			telemetry.Attr(cliusage.AttrCommands, telemetry.StringVal("build,test")),
			telemetry.Attr(cliusage.AttrProjects, telemetry.IntVal(3)),
			telemetry.Attr(cliusage.AttrJobs, telemetry.IntVal(9)),
			telemetry.Attr(cliusage.AttrInteractive, telemetry.BoolVal(true)),
			telemetry.Attr(cliusage.AttrFlagImpacted, telemetry.BoolVal(false)),
			telemetry.Attr(cliusage.AttrFlagCoverage, telemetry.BoolVal(false)),
			telemetry.Attr(cliusage.AttrFlagOutput, telemetry.BoolVal(false)),
			telemetry.Attr(cliusage.AttrFlagNoCache, telemetry.BoolVal(false)),
			telemetry.Attr(cliusage.AttrFlagProjects, telemetry.BoolVal(false)),
			telemetry.Attr(cliusage.AttrFlagWatch, telemetry.BoolVal(false)),
		},
	}
}

// recordAttrs returns the sole record of a sanitized request as a key→value map,
// failing if the shape is not exactly one resource/scope/record.
func soleRecord(t *testing.T, clean telemetry.LogsRequest) telemetry.LogRecord {
	t.Helper()
	if len(clean.ResourceLogs) != 1 || len(clean.ResourceLogs[0].ScopeLogs) != 1 ||
		len(clean.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("expected exactly one record, got %+v", clean)
	}
	return clean.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
}

func resourceAttr(res telemetry.Resource, key string) (telemetry.AnyValue, bool) {
	for _, kv := range res.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return telemetry.AnyValue{}, false
}

// --- tests ----------------------------------------------------------------

// TestSanitizeGoldenRoundTrip is the core invariant: the S2a golden fixture
// survives the sanitizer unchanged (record byte-for-byte identical) while the
// resource gains an unspoofable origin=cli-anon, and the result still conforms
// to the shared vocabulary.
func TestSanitizeGoldenRoundTrip(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "fields-are-allowlisted", "the-compiled-allowlist-reduction-is-pinned")
	req, diags := telemetry.ParseAndValidateLogs(cliusage.GoldenLogsJSON)
	if diag.HasErrors(diags) {
		t.Fatalf("golden failed structural OTLP validation: %v", diags)
	}

	clean := sanitizeLogs(req)

	if got := cliusage.ValidateLogs(clean); diag.HasErrors(got) {
		t.Fatalf("sanitized golden failed CLI usage vocabulary: %v", got)
	}

	golden := req.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	got := soleRecord(t, clean)
	if !reflect.DeepEqual(got, golden) {
		t.Fatalf("record changed by sanitizer:\n got  %+v\n want %+v", got, golden)
	}

	origin, ok := resourceAttr(clean.ResourceLogs[0].Resource, attrOrigin)
	if !ok || origin.StringValue == nil || *origin.StringValue != originCLIAnon {
		t.Fatalf("origin not stamped to %q: %+v", originCLIAnon, clean.ResourceLogs[0].Resource)
	}
	svc, ok := resourceAttr(clean.ResourceLogs[0].Resource, telemetry.AttrServiceName)
	if !ok || svc.StringValue == nil || *svc.StringValue != cliusage.ServiceName {
		t.Fatalf("service.name not preserved: %+v", clean.ResourceLogs[0].Resource)
	}
}

func TestSanitizeValidStartPassesThrough(t *testing.T) {
	clean := sanitizeLogs(logsReq(cliResource(), validStartRecord()))
	if got := cliusage.ValidateLogs(clean); diag.HasErrors(got) {
		t.Fatalf("valid session:start rejected: %v", got)
	}
	_ = soleRecord(t, clean)
}

// TestSanitizeStampsOriginUnspoofable proves a caller cannot set origin, whether
// supplied at the resource level or as a record attribute.
func TestSanitizeStampsOriginUnspoofable(t *testing.T) {
	// origin injected at the resource level.
	res := cliResource(telemetry.Attr(attrOrigin, telemetry.StringVal("attacker-controlled")))
	clean := sanitizeLogs(logsReq(res, validStartRecord()))
	origin, ok := resourceAttr(clean.ResourceLogs[0].Resource, attrOrigin)
	if !ok || origin.StringValue == nil || *origin.StringValue != originCLIAnon {
		t.Fatalf("resource-level origin not overwritten: %+v", clean.ResourceLogs[0].Resource)
	}

	// origin injected as a record attribute must be stripped (unknown key).
	rec := validStartRecord()
	rec.Attributes = append(rec.Attributes, telemetry.Attr(attrOrigin, telemetry.StringVal("attacker")))
	clean = sanitizeLogs(logsReq(cliResource(), rec))
	got := soleRecord(t, clean)
	for _, kv := range got.Attributes {
		if kv.Key == attrOrigin {
			t.Fatalf("record-level origin was not stripped: %+v", got.Attributes)
		}
	}
}

// TestSanitizeRemovesEveryOuterFreeTextSurface enumerates the string-bearing
// OTLP envelope fields outside the closed record vocabulary. None may preserve
// attacker input, even when the underlying record is otherwise valid.
func TestSanitizeRemovesEveryOuterFreeTextSurface(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "fields-are-allowlisted", "every-free-text-surface-is-removed")
	const injected = "customer@example.com /private/workspace"
	res := cliResource(
		telemetry.Attr(telemetry.AttrServiceVersion, telemetry.StringVal(injected)),
		telemetry.Attr(telemetry.AttrPutnamiFramework, telemetry.StringVal(injected)),
		telemetry.Attr("host.name", telemetry.StringVal(injected)),
	)
	req := logsReq(res, validStartRecord())
	req.ResourceLogs[0].ScopeLogs[0].Scope = telemetry.Scope{Name: injected, Version: injected}
	rec := &req.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	rec.SeverityText = injected
	rec.SeverityNumber = 24
	body := telemetry.StringVal(injected)
	rec.Body = &body

	clean := sanitizeLogs(req)
	got := soleRecord(t, clean)
	resource := clean.ResourceLogs[0].Resource
	scope := clean.ResourceLogs[0].ScopeLogs[0].Scope

	var surfaces []string
	for _, kv := range resource.Attributes {
		if kv.Value.StringValue != nil {
			surfaces = append(surfaces, kv.Key, *kv.Value.StringValue)
		}
	}
	surfaces = append(surfaces, scope.Name, scope.Version, got.SeverityText)
	if got.Body != nil && got.Body.StringValue != nil {
		surfaces = append(surfaces, *got.Body.StringValue)
	}
	for _, kv := range got.Attributes {
		surfaces = append(surfaces, kv.Key)
		if kv.Value.StringValue != nil {
			surfaces = append(surfaces, *kv.Value.StringValue)
		}
	}
	for _, surface := range surfaces {
		if strings.Contains(surface, injected) {
			t.Fatalf("caller free text survived in sanitized output surface %q", surface)
		}
	}
	if scope.Name != cliusage.ScopeName || scope.Version != "" {
		t.Fatalf("scope = %+v, want pinned name and no invalid version", scope)
	}
	if got.SeverityNumber != telemetry.SeverityInfo || got.SeverityText != "info" {
		t.Fatalf("severity = (%d, %q), want receiver-pinned info", got.SeverityNumber, got.SeverityText)
	}
	if got.Body == nil || got.Body.StringValue == nil || *got.Body.StringValue != cliusage.EventSessionStart {
		t.Fatalf("body = %+v, want event-derived %q", got.Body, cliusage.EventSessionStart)
	}
}

func TestSanitizeStripsUnknownAttributeKeys(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "fields-are-allowlisted", "an-unknown-field-is-dropped-before-storage")
	rec := validStartRecord()
	rec.Attributes = append(rec.Attributes,
		telemetry.Attr("totally.unknown", telemetry.StringVal("x")),
		telemetry.Attr("secret.token", telemetry.StringVal("hunter2")),
	)
	clean := sanitizeLogs(logsReq(cliResource(), rec))
	got := soleRecord(t, clean)
	for _, kv := range got.Attributes {
		if kv.Key == "totally.unknown" || kv.Key == "secret.token" {
			t.Fatalf("unknown key survived: %q", kv.Key)
		}
	}
	// The record is still valid after stripping (the stripped keys were extras).
	if d := cliusage.ValidateRecord(got); diag.HasErrors(d) {
		t.Fatalf("stripped record no longer valid: %v", d)
	}
}

// TestSanitizeDropsNonConforming covers the fail-silent drops: any record that
// cannot be normalized is removed, never surfaced as an error.
func TestSanitizeDropsNonConforming(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "fields-are-allowlisted", "a-record-outside-the-closed-vocabulary-is-dropped")
	sessionEnd := func(mutate func(*telemetry.LogRecord)) telemetry.LogRecord {
		rec := telemetry.LogRecord{
			SeverityNumber: telemetry.SeverityInfo,
			Attributes: []telemetry.KeyValue{
				telemetry.Attr(cliusage.AttrEventName, telemetry.StringVal(cliusage.EventSessionEnd)),
				telemetry.Attr(cliusage.AttrDeviceID, telemetry.StringVal("0123456789abcdef0123456789abcdef")),
				telemetry.Attr(cliusage.AttrCLIVersion, telemetry.StringVal("v1.0.0")),
				telemetry.Attr(cliusage.AttrOS, telemetry.StringVal("darwin")),
				telemetry.Attr(cliusage.AttrArch, telemetry.StringVal("arm64")),
				telemetry.Attr(cliusage.AttrSuccess, telemetry.BoolVal(false)),
				telemetry.Attr(cliusage.AttrDuration, telemetry.IntVal(42)),
				telemetry.Attr(cliusage.AttrInteractive, telemetry.BoolVal(false)),
				telemetry.Attr(cliusage.AttrErrorCategory, telemetry.StringVal(cliusage.ErrorCategoryFailure)),
			},
		}
		if mutate != nil {
			mutate(&rec)
		}
		return rec
	}

	cases := []struct {
		name string
		req  *telemetry.LogsRequest
	}{
		{
			name: "unknown event name",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				r.Attributes[0] = telemetry.Attr(cliusage.AttrEventName, telemetry.StringVal("session:teleport"))
				return r
			}()),
		},
		{
			name: "non-string event name",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				r.Attributes[0] = telemetry.Attr(cliusage.AttrEventName, telemetry.BoolVal(true))
				return r
			}()),
		},
		{
			name: "invalid error category enum",
			req: logsReq(cliResource(), sessionEnd(func(r *telemetry.LogRecord) {
				r.Attributes[8] = telemetry.Attr(cliusage.AttrErrorCategory, telemetry.StringVal("meltdown"))
			})),
		},
		{
			name: "wrong value kind for int attribute",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				// projects must be int; send a bool → attr dropped → required missing.
				r.Attributes[6] = telemetry.Attr(cliusage.AttrProjects, telemetry.BoolVal(true))
				return r
			}()),
		},
		{
			name: "success consistency violation (failure with no category)",
			req:  logsReq(cliResource(), sessionEnd(func(r *telemetry.LogRecord) { r.Attributes = r.Attributes[:8] })),
		},
		{
			name: "control char in required string attribute",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				r.Attributes[5] = telemetry.Attr(cliusage.AttrCommands, telemetry.StringVal("build\x00test"))
				return r
			}()),
		},
		{
			name: "syntactically valid unknown command",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				r.Attributes[5] = telemetry.Attr(
					cliusage.AttrCommands,
					telemetry.StringVal("attacker-command"),
				)
				return r
			}()),
		},
		{
			name: "free text device id",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				r.Attributes[1] = telemetry.Attr(cliusage.AttrDeviceID, telemetry.StringVal("customer@example.com"))
				return r
			}()),
		},
		{
			name: "free text cli version",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				r.Attributes[2] = telemetry.Attr(cliusage.AttrCLIVersion, telemetry.StringVal("/tmp/local-build"))
				return r
			}()),
		},
		{
			name: "unknown os",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				r.Attributes[3] = telemetry.Attr(cliusage.AttrOS, telemetry.StringVal("workstation"))
				return r
			}()),
		},
		{
			name: "unknown arch",
			req: logsReq(cliResource(), func() telemetry.LogRecord {
				r := validStartRecord()
				r.Attributes[4] = telemetry.Attr(cliusage.AttrArch, telemetry.StringVal("fast-cpu"))
				return r
			}()),
		},
		{
			name: "resource is not cli-usage (masquerade)",
			req: logsReq(
				telemetry.Resource{Attributes: []telemetry.KeyValue{telemetry.Attr(telemetry.AttrServiceName, telemetry.StringVal("evil-service"))}},
				validStartRecord(),
			),
		},
		{
			name: "resource missing service.name",
			req:  logsReq(telemetry.Resource{}, validStartRecord()),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean := sanitizeLogs(tc.req)
			if len(clean.ResourceLogs) != 0 {
				t.Fatalf("expected all records dropped, got %+v", clean)
			}
			// Whatever survives (nothing) must still validate — never leaks junk.
			if d := cliusage.ValidateLogs(clean); diag.HasErrors(d) {
				t.Fatalf("sanitized output not clean: %v", d)
			}
		})
	}
}

// TestSanitizeRebuildsIntFormatting proves values are rebuilt from their typed
// scalar, not passed through: a non-canonical integer string is re-formatted.
func TestSanitizeRebuildsIntFormatting(t *testing.T) {
	rec := validStartRecord()
	weird := "007"
	rec.Attributes[6] = telemetry.Attr(cliusage.AttrProjects, telemetry.AnyValue{IntValue: &weird})
	clean := sanitizeLogs(logsReq(cliResource(), rec))
	got := soleRecord(t, clean)
	for _, kv := range got.Attributes {
		if kv.Key == cliusage.AttrProjects {
			if kv.Value.IntValue == nil || *kv.Value.IntValue != "7" {
				t.Fatalf("projects not re-canonicalized: %+v", kv.Value)
			}
			return
		}
	}
	t.Fatal("projects attribute missing")
}
