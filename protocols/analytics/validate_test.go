package analytics

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	testEventID   = "0199116c-8f00-7a1b-8c2d-3e4f5a6b7c8d"
	testSessionID = "0199116c-8e00-7a1b-8c2d-3e4f5a6b7c00"
	testTimestamp = "2026-09-02T10:00:00.000Z"
)

func validPageView() Event {
	return Event{
		EventID:   testEventID,
		Name:      EventPageView,
		ClientTs:  testTimestamp,
		Seq:       0,
		SessionID: testSessionID,
		Page:      &Page{Path: "/"},
	}
}

func validAction() Event {
	return Event{
		EventID:   testEventID,
		Name:      EventAction,
		ClientTs:  testTimestamp,
		Seq:       1,
		SessionID: testSessionID,
		Action:    "signup_click",
		Props:     map[string]any{"plan": "pro", "seats": float64(3), "trial": true},
	}
}

func batchOf(events ...Event) *Batch {
	return &Batch{ProtocolVersion: ProtocolVersion, SentAt: testTimestamp, Events: events}
}

func intPtr(v int) *int { return &v }

// TestValidateBatch_HappyPath pins that both event kinds pass unmodified. Every
// negative case below is one mutation away from these, so a false positive here
// would make the whole table meaningless.
func TestValidateBatch_HappyPath(t *testing.T) {
	for name, event := range map[string]Event{"page_view": validPageView(), "action": validAction()} {
		t.Run(name, func(t *testing.T) {
			if diags := ValidateBatch(batchOf(event)); len(diags) != 0 {
				t.Errorf("valid %s batch produced %v", name, diags)
			}
		})
	}
	full := validPageView()
	full.EngagementMs = intPtr(12500)
	full.ViewportClass = "lg"
	full.Language = "fr-FR"
	full.Page = &Page{
		Path:     "/tasks/42",
		Route:    "/tasks/[id]",
		Referrer: "https://www.google.com/search?q=putnami",
		UTM:      map[string]string{"source": "google", "medium": "cpc"},
	}
	if diags := ValidateBatch(batchOf(full)); len(diags) != 0 {
		t.Errorf("fully populated page view produced %v", diags)
	}
}

// TestValidateBatch_Envelope covers the three envelope rules of the contract.
func TestValidateBatch_Envelope(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Batch)
		wantCode  string
		wantField string
	}{
		{"version", func(b *Batch) { b.ProtocolVersion = 2 }, ErrorCodeInvalidVersion, "protocolVersion"},
		{"version zero", func(b *Batch) { b.ProtocolVersion = 0 }, ErrorCodeInvalidVersion, "protocolVersion"},
		{"sentAt", func(b *Batch) { b.SentAt = "2026-09-02" }, ErrorCodeInvalidTimestamp, "sentAt"},
		{"sentAt without milliseconds", func(b *Batch) { b.SentAt = "2026-09-02T10:00:00Z" }, ErrorCodeInvalidTimestamp, "sentAt"},
		{"no events", func(b *Batch) { b.Events = nil }, ErrorCodeBatchTooLarge, "events"},
		{"too many events", func(b *Batch) {
			b.Events = make([]Event, MaxEvents+1)
			for i := range b.Events {
				b.Events[i] = validPageView()
			}
		}, ErrorCodeBatchTooLarge, "events"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch := batchOf(validPageView())
			test.mutate(batch)
			requireDiagnostic(t, ValidateBatch(batch), test.wantCode, test.wantField)
		})
	}
}

// TestValidateBatch_MaxEventsIsAccepted pins the inclusive upper bound: a batch
// of exactly MaxEvents is the size ceiling, not the first rejection.
func TestValidateBatch_MaxEventsIsAccepted(t *testing.T) {
	events := make([]Event, MaxEvents)
	for i := range events {
		events[i] = validPageView()
	}
	if diags := ValidateBatch(batchOf(events...)); len(diags) != 0 {
		t.Errorf("a batch of exactly %d events produced %v", MaxEvents, diags)
	}
}

// TestValidateBatch_NilIsARefusal keeps the only nil-input path from returning
// "conforms" — a caller that ignored a parse failure must not read silence as
// acceptance.
func TestValidateBatch_NilIsARefusal(t *testing.T) {
	requireDiagnostic(t, ValidateBatch(nil), ErrorCodeParseError, "")
}

// TestValidateBatch_FieldPathsCarryTheEventIndex pins that a diagnostic points
// at the offending event, not merely at the batch: a sanitizer drops per event.
func TestValidateBatch_FieldPathsCarryTheEventIndex(t *testing.T) {
	bad := validPageView()
	bad.EventID = "not-a-uuid"
	requireDiagnostic(t, ValidateBatch(batchOf(validPageView(), bad)), ErrorCodeInvalidEventID, "events[1].eventId")
}

// TestValidateEvent_Rules walks the rule table of the contract, one case per
// row. Each case names the exact code and field path a consumer branches on.
func TestValidateEvent_Rules(t *testing.T) {
	tests := []struct {
		name      string
		event     Event
		mutate    func(*Event)
		wantCode  string
		wantField string
	}{
		{"missing eventId", validPageView(), func(e *Event) { e.EventID = "" }, ErrorCodeMissingAttribute, "events[0].eventId"},
		{"missing name", validPageView(), func(e *Event) { e.Name = "" }, ErrorCodeMissingAttribute, "events[0].name"},
		{"missing clientTs", validPageView(), func(e *Event) { e.ClientTs = "" }, ErrorCodeMissingAttribute, "events[0].clientTs"},
		{"missing sessionId", validPageView(), func(e *Event) { e.SessionID = "" }, ErrorCodeMissingAttribute, "events[0].sessionId"},

		{"eventId is a v4 uuid", validPageView(), func(e *Event) {
			e.EventID = "0199116c-8f00-4a1b-8c2d-3e4f5a6b7c8d"
		}, ErrorCodeInvalidEventID, "events[0].eventId"},
		{"eventId is uppercase", validPageView(), func(e *Event) {
			e.EventID = strings.ToUpper(testEventID)
		}, ErrorCodeInvalidEventID, "events[0].eventId"},
		{"sessionId is malformed", validPageView(), func(e *Event) {
			e.SessionID = "0199116c8e007a1b8c2d3e4f5a6b7c00"
		}, ErrorCodeInvalidEventID, "events[0].sessionId"},

		{"server-only event name", validPageView(), func(e *Event) { e.Name = EventFormSubmit }, ErrorCodeUnknownEvent, "events[0].name"},
		{"invented event name", validPageView(), func(e *Event) { e.Name = "click" }, ErrorCodeUnknownEvent, "events[0].name"},
		{"clientTs without milliseconds", validPageView(), func(e *Event) {
			e.ClientTs = "2026-09-02T10:00:00Z"
		}, ErrorCodeInvalidTimestamp, "events[0].clientTs"},

		{"negative seq", validPageView(), func(e *Event) { e.Seq = -1 }, ErrorCodeInvalidValue, "events[0].seq"},
		{"seq over the ceiling", validPageView(), func(e *Event) { e.Seq = MaxSeq + 1 }, ErrorCodeInvalidValue, "events[0].seq"},
		{"negative engagement", validPageView(), func(e *Event) { e.EngagementMs = intPtr(-1) }, ErrorCodeInvalidValue, "events[0].engagementMs"},
		{"engagement over a day", validPageView(), func(e *Event) {
			e.EngagementMs = intPtr(MaxEngagementMs + 1)
		}, ErrorCodeInvalidValue, "events[0].engagementMs"},
		{"unknown viewport class", validPageView(), func(e *Event) { e.ViewportClass = "xxl" }, ErrorCodeInvalidValue, "events[0].viewportClass"},
		{"malformed language", validPageView(), func(e *Event) { e.Language = "french" }, ErrorCodeInvalidValue, "events[0].language"},

		{"page view without page", validPageView(), func(e *Event) { e.Page = nil }, ErrorCodeMissingAttribute, "events[0].page"},
		{"page view with an action", validPageView(), func(e *Event) { e.Action = "signup_click" }, ErrorCodeUnknownAttribute, "events[0].action"},
		{"page view with props", validPageView(), func(e *Event) {
			e.Props = map[string]any{"plan": "pro"}
		}, ErrorCodeUnknownAttribute, "events[0].props"},
		{"action with a page", validAction(), func(e *Event) { e.Page = &Page{Path: "/"} }, ErrorCodeUnknownAttribute, "events[0].page"},
		{"action without a name", validAction(), func(e *Event) { e.Action = "" }, ErrorCodeMissingAttribute, "events[0].action"},
		{"action name with spaces", validAction(), func(e *Event) { e.Action = "Sign Up" }, ErrorCodeInvalidAction, "events[0].action"},
		{"action name too long", validAction(), func(e *Event) {
			e.Action = strings.Repeat("a", MaxActionNameLen+1)
		}, ErrorCodeInvalidAction, "events[0].action"},

		{"empty path", validPageView(), func(e *Event) { e.Page.Path = "" }, ErrorCodeInvalidPath, "events[0].page.path"},
		{"relative path", validPageView(), func(e *Event) { e.Page.Path = "tasks" }, ErrorCodeInvalidPath, "events[0].page.path"},
		{"path with a query", validPageView(), func(e *Event) { e.Page.Path = "/a?b=1" }, ErrorCodeInvalidPath, "events[0].page.path"},
		{"path with a fragment", validPageView(), func(e *Event) { e.Page.Path = "/a#b" }, ErrorCodeInvalidPath, "events[0].page.path"},
		{"path too long", validPageView(), func(e *Event) {
			e.Page.Path = "/" + strings.Repeat("a", MaxPathLen)
		}, ErrorCodeInvalidPath, "events[0].page.path"},

		{"colon route", validPageView(), func(e *Event) { e.Page.Route = "/tasks/:id" }, ErrorCodeInvalidRoute, "events[0].page.route"},
		{"route without a leading slash", validPageView(), func(e *Event) { e.Page.Route = "tasks" }, ErrorCodeInvalidRoute, "events[0].page.route"},
		{"route too long", validPageView(), func(e *Event) {
			e.Page.Route = "/" + strings.Repeat("a", MaxRouteLen)
		}, ErrorCodeInvalidRoute, "events[0].page.route"},

		{"javascript referrer", validPageView(), func(e *Event) {
			e.Page.Referrer = "javascript:alert(1)"
		}, ErrorCodeInvalidReferrer, "events[0].page.referrer"},
		{"scheme-relative referrer", validPageView(), func(e *Event) {
			e.Page.Referrer = "//evil.example.com/x"
		}, ErrorCodeInvalidReferrer, "events[0].page.referrer"},
		{"backslash-relative referrer", validPageView(), func(e *Event) {
			e.Page.Referrer = `/\evil.example.com/x`
		}, ErrorCodeInvalidReferrer, "events[0].page.referrer"},
		{"http referrer without a host", validPageView(), func(e *Event) {
			e.Page.Referrer = "https:///path"
		}, ErrorCodeInvalidReferrer, "events[0].page.referrer"},
		{"referrer too long", validPageView(), func(e *Event) {
			e.Page.Referrer = "https://example.com/" + strings.Repeat("a", MaxReferrerLen)
		}, ErrorCodeInvalidReferrer, "events[0].page.referrer"},

		{"unknown utm key", validPageView(), func(e *Event) {
			e.Page.UTM = map[string]string{"campaign_id": "x"}
		}, ErrorCodeInvalidUTM, "events[0].page.utm.campaign_id"},
		{"empty utm value", validPageView(), func(e *Event) {
			e.Page.UTM = map[string]string{"source": ""}
		}, ErrorCodeInvalidUTM, "events[0].page.utm.source"},
		{"utm value too long", validPageView(), func(e *Event) {
			e.Page.UTM = map[string]string{"term": strings.Repeat("a", MaxUTMLen+1)}
		}, ErrorCodeInvalidUTM, "events[0].page.utm.term"},

		{"too many props", validAction(), func(e *Event) {
			e.Props = make(map[string]any, MaxPropKeys+1)
			for i := 0; i <= MaxPropKeys; i++ {
				e.Props[fmt.Sprintf("key_%02d", i)] = float64(i)
			}
		}, ErrorCodePropsTooMany, "events[0].props"},
		{"uppercase prop key", validAction(), func(e *Event) {
			e.Props = map[string]any{"Plan": "pro"}
		}, ErrorCodeInvalidPropKey, "events[0].props.Plan"},
		{"prop key starting with a digit", validAction(), func(e *Event) {
			e.Props = map[string]any{"1st": "pro"}
		}, ErrorCodeInvalidPropKey, "events[0].props.1st"},
		{"nested prop value", validAction(), func(e *Event) {
			e.Props = map[string]any{"plan": map[string]any{"nested": true}}
		}, ErrorCodeInvalidPropValue, "events[0].props.plan"},
		{"null prop value", validAction(), func(e *Event) {
			e.Props = map[string]any{"plan": nil}
		}, ErrorCodeInvalidPropValue, "events[0].props.plan"},
		{"array prop value", validAction(), func(e *Event) {
			e.Props = map[string]any{"plan": []any{"pro"}}
		}, ErrorCodeInvalidPropValue, "events[0].props.plan"},
		{"prop string too long", validAction(), func(e *Event) {
			e.Props = map[string]any{"plan": strings.Repeat("a", MaxPropStringLen+1)}
		}, ErrorCodeInvalidPropValue, "events[0].props.plan"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := test.event
			test.mutate(&event)
			requireDiagnostic(t, ValidateEvent(event, "events[0]"), test.wantCode, test.wantField)
		})
	}
}

// TestValidateEvent_AcceptsBoundaryValues pins the inclusive edges of every
// numeric bound, so a later "off by one" tightening fails here rather than
// dropping real traffic in production.
func TestValidateEvent_AcceptsBoundaryValues(t *testing.T) {
	event := validPageView()
	event.Seq = MaxSeq
	event.EngagementMs = intPtr(MaxEngagementMs)
	event.ViewportClass = "xs"
	event.Language = "en"
	event.Page = &Page{
		Path:     "/" + strings.Repeat("a", MaxPathLen-1),
		Route:    "/" + strings.Repeat("a", MaxRouteLen-1),
		Referrer: "/back",
		UTM:      map[string]string{"campaign": strings.Repeat("a", MaxUTMLen)},
	}
	if diags := ValidateEvent(event, "events[0]"); len(diags) != 0 {
		t.Errorf("boundary values produced %v", diags)
	}

	action := validAction()
	action.Props = make(map[string]any, MaxPropKeys)
	for i := range MaxPropKeys {
		action.Props[fmt.Sprintf("key_%02d", i)] = strings.Repeat("a", MaxPropStringLen)
	}
	if diags := ValidateEvent(action, "events[0]"); len(diags) != 0 {
		t.Errorf("exactly %d props produced %v", MaxPropKeys, diags)
	}
}

// TestValidateEvent_PropValueKinds pins the three accepted scalar kinds and the
// finiteness rule. NaN and infinities cannot arrive over JSON, but a Go caller
// building a Batch by hand can produce them, and a non-finite value would break
// the counter fold on the receiving side rather than being rejected at the edge.
func TestValidateEvent_PropValueKinds(t *testing.T) {
	accepted := map[string]any{
		"text":    "pro",
		"count":   float64(3),
		"flag":    true,
		"decoded": json.Number("42"),
		"zero":    float64(0),
		"empty":   "",
	}
	action := validAction()
	action.Props = accepted
	if diags := ValidateEvent(action, "events[0]"); len(diags) != 0 {
		t.Errorf("accepted prop kinds produced %v", diags)
	}

	rejected := map[string]any{
		"nan":        math.NaN(),
		"inf":        math.Inf(1),
		"neginf":     math.Inf(-1),
		"unparsed":   json.Number("not-a-number"),
		"structured": map[string]any{"a": 1},
	}
	for key, value := range rejected {
		t.Run(key, func(t *testing.T) {
			event := validAction()
			event.Props = map[string]any{"plan": value}
			requireDiagnostic(t, ValidateEvent(event, "events[0]"), ErrorCodeInvalidPropValue, "events[0].props.plan")
		})
	}
}

// TestValidateEvent_AcceptsEveryRouteShape pins the route grammar against the
// three segment kinds a Putnami router produces plus the root route.
func TestValidateEvent_AcceptsEveryRouteShape(t *testing.T) {
	for _, route := range []string{"/", "/tasks", "/tasks/[id]", "/docs/[...slug]", "/a-b_c.d~e/[id]/edit"} {
		event := validPageView()
		event.Page = &Page{Path: "/x", Route: route}
		if diags := ValidateEvent(event, "events[0]"); len(diags) != 0 {
			t.Errorf("route %q produced %v", route, diags)
		}
	}
}

// TestValidateEvent_AcceptsEveryReferrerShape pins the two accepted referrer
// forms: an absolute http(s) URL and an app-relative path.
func TestValidateEvent_AcceptsEveryReferrerShape(t *testing.T) {
	for _, referrer := range []string{"https://example.com/a", "http://example.com", "/internal/page"} {
		event := validPageView()
		event.Page = &Page{Path: "/x", Referrer: referrer}
		if diags := ValidateEvent(event, "events[0]"); len(diags) != 0 {
			t.Errorf("referrer %q produced %v", referrer, diags)
		}
	}
}

// TestValidateEvent_IsDeterministicOverMaps pins that diagnostics derived from
// the utm and props maps come out in the same order every run. Go randomizes map
// iteration, so without the sort a consumer diffing two validation runs of the
// same document would see spurious churn.
func TestValidateEvent_IsDeterministicOverMaps(t *testing.T) {
	event := validPageView()
	event.Page = &Page{Path: "/x", UTM: map[string]string{"zzz": "1", "aaa": "2", "mmm": "3"}}
	event.Props = map[string]any{"Zed": nil, "Abe": nil, "Mid": nil}

	first := fieldsOf(ValidateEvent(event, "events[0]"))
	for range 20 {
		if got := fieldsOf(ValidateEvent(event, "events[0]")); !equalStrings(got, first) {
			t.Fatalf("diagnostic order is not deterministic: %v then %v", first, got)
		}
	}
}

// TestParseBatchStrict_ClassifiesDecodeFailures pins the three decode outcomes
// the contract distinguishes. Flattening them into parse_error would leave a
// sanitizer unable to tell a hostile body from a version skew.
func TestParseBatchStrict_ClassifiesDecodeFailures(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"not json", "<html>", ErrorCodeParseError},
		{"truncated", `{"protocolVersion": 1, "events": [`, ErrorCodeParseError},
		{"empty body", "", ErrorCodeParseError},
		{"wrong type", `{"protocolVersion": 1, "sentAt": "` + testTimestamp + `", "events": [{"seq": "1"}]}`, ErrorCodeAttributeKind},
		{"unknown envelope field", `{"protocolVersion": 1, "extra": true}`, ErrorCodeUnknownAttribute},
		{"unknown event field", `{"protocolVersion": 1, "events": [{"title": "x"}]}`, ErrorCodeUnknownAttribute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch, diags := ParseBatchStrict([]byte(test.body))
			if batch != nil {
				t.Errorf("a rejected body must not be returned, got %#v", batch)
			}
			if len(diags) != 1 {
				t.Fatalf("got %d diagnostics, want exactly one: %v", len(diags), diags)
			}
			if diags[0].Code != test.wantCode {
				t.Errorf("code = %q, want %q (%s)", diags[0].Code, test.wantCode, diags[0].Message)
			}
			if !ValidErrorCodes[diags[0].Code] {
				t.Errorf("code %q is outside the closed vocabulary", diags[0].Code)
			}
		})
	}
}

// TestParseBatchStrict_NamesTheUnknownField pins that the offending name reaches
// the diagnostic. The standard library only puts it in the message text, so this
// is the one place the extraction is checked.
func TestParseBatchStrict_NamesTheUnknownField(t *testing.T) {
	_, diags := ParseBatchStrict([]byte(`{"protocolVersion": 1, "events": [{"title": "x"}]}`))
	if len(diags) != 1 || diags[0].Field != "title" {
		t.Fatalf("diagnostics = %v, want one naming field %q", diags, "title")
	}
}

// TestParseAndValidateBatch_StopsAtDecodeFailure pins that a body that does not
// decode never reaches the validator: validating a half-decoded batch would
// report rule violations the sender cannot act on.
func TestParseAndValidateBatch_StopsAtDecodeFailure(t *testing.T) {
	batch, diags := ParseAndValidateBatch([]byte("not json"))
	if batch != nil {
		t.Errorf("batch = %#v, want nil", batch)
	}
	if len(diags) != 1 || diags[0].Code != ErrorCodeParseError {
		t.Fatalf("diagnostics = %v, want exactly one parse_error", diags)
	}
}

// TestParseAndValidateBatch_ReportsRuleViolations pins the composed path: a
// decodable body that breaks a rule comes back with the rule's code.
func TestParseAndValidateBatch_ReportsRuleViolations(t *testing.T) {
	body := `{"protocolVersion": 2, "sentAt": "` + testTimestamp + `", "events": []}`
	batch, diags := ParseAndValidateBatch([]byte(body))
	if batch == nil {
		t.Fatal("a decodable batch must be returned even when it violates rules")
	}
	requireDiagnostic(t, diags, ErrorCodeInvalidVersion, "protocolVersion")
	requireDiagnostic(t, diags, ErrorCodeBatchTooLarge, "events")
}

// TestParseAndValidateBatch_AcceptsTheMinimalBatch closes the loop on the
// smallest conforming document.
func TestParseAndValidateBatch_AcceptsTheMinimalBatch(t *testing.T) {
	body := `{"protocolVersion": 1, "sentAt": "` + testTimestamp + `",
		"events": [{"eventId": "` + testEventID + `", "name": "page_view", "clientTs": "` + testTimestamp + `",
		"seq": 0, "sessionId": "` + testSessionID + `", "page": {"path": "/"}}]}`
	batch, diags := ParseAndValidateBatch([]byte(body))
	if diag.HasErrors(diags) {
		t.Fatalf("the minimal batch produced %v", diags)
	}
	if batch == nil || len(batch.Events) != 1 {
		t.Fatalf("batch = %#v, want one event", batch)
	}
}

// requireDiagnostic asserts that diags carries an error with the given code and
// field, and that the code is inside the closed vocabulary.
func requireDiagnostic(t *testing.T, diags []diag.Diagnostic, code, field string) {
	t.Helper()
	if !ValidErrorCodes[code] {
		t.Fatalf("test asks for %q, which is outside ValidErrorCodes", code)
	}
	for _, d := range diags {
		if d.Code == code && d.Field == field {
			if d.Severity != diag.Error {
				t.Errorf("%s on %s has severity %q, want error", code, field, d.Severity)
			}
			if d.Message == "" {
				t.Errorf("%s on %s carries no message", code, field)
			}
			return
		}
	}
	t.Errorf("no diagnostic with code %q on field %q; got %v", code, field, diags)
}

func fieldsOf(diags []diag.Diagnostic) []string {
	fields := make([]string, 0, len(diags))
	for _, d := range diags {
		fields = append(fields, d.Field)
	}
	return fields
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestValidatePageBoundsAreBytes pins the unit of every string bound: UTF-8
// bytes, not characters. The distinction is invisible in an ASCII-only corpus
// and decides accept-vs-reject across languages — a sanitizer measuring UTF-16
// code units would accept a 300-character Cyrillic path this package rejects at
// 600 bytes. The README states the rule; these cases are what make it binding.
func TestValidatePageBoundsAreBytes(t *testing.T) {
	// "я" is two bytes in UTF-8 and one JavaScript string unit.
	const twoByteRune = "я"

	atBound := "/" + strings.Repeat(twoByteRune, (MaxPathLen-1)/2)
	overBound := "/" + strings.Repeat(twoByteRune, (MaxPathLen+1)/2)

	// Both cases must stay under the bound when counted the wrong way, or they
	// would fail for a rune-counting implementation too and prove nothing.
	for _, path := range []string{atBound, overBound} {
		if runes := len([]rune(path)); runes > MaxPathLen {
			t.Fatalf("case is vacuous: %d runes already exceeds %d", runes, MaxPathLen)
		}
	}

	accepted := validPageView()
	accepted.Page.Path = atBound
	if diags := ValidateEvent(accepted, "events[0]"); len(diags) != 0 {
		t.Errorf("a path of %d bytes must be accepted at a bound of %d, got %v",
			len(atBound), MaxPathLen, diags)
	}

	rejected := validPageView()
	rejected.Page.Path = overBound
	requireDiagnostic(t, ValidateEvent(rejected, "events[0]"), ErrorCodeInvalidPath, "events[0].page.path")
}

// TestValidateUTMBoundIsBytes pins the same unit on a campaign value, so the
// rule is not pinned only where the path happens to enforce it.
func TestValidateUTMBoundIsBytes(t *testing.T) {
	value := strings.Repeat("я", MaxUTMLen/2+1)
	if runes := len([]rune(value)); runes > MaxUTMLen {
		t.Fatalf("case is vacuous: %d runes already exceeds %d", runes, MaxUTMLen)
	}

	event := validPageView()
	event.Page.UTM = map[string]string{"source": value}
	requireDiagnostic(t, ValidateEvent(event, "events[0]"), ErrorCodeInvalidUTM, "events[0].page.utm.source")
}
