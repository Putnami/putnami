package events

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ManagedPublishContractVersion is the managed Event Server admission/config
// compatibility version. It is independent from the putnami.events.v1 wire
// protocol identifier.
const ManagedPublishContractVersion = 1

// ManagedPublishFixtureRoot is the path of the canonical managed-publish v1
// corpus within this module.
const ManagedPublishFixtureRoot = "fixtures/managed-publish/v1"

// PublishRef is the canonical successful HTTP publish response. ID is the
// stable request ID supplied by a managed publisher; Topic remains logical.
type PublishRef struct {
	// Protocol is the events protocol identifier (Protocol).
	Protocol string `json:"protocol"`
	// ID echoes the stable request ID supplied by the managed publisher.
	ID string `json:"id"`
	// Topic echoes the logical topic the event was published to.
	Topic string `json:"topic"`
	// Timestamp is the RFC3339Nano acceptance time stamped by the Event Server.
	Timestamp string `json:"timestamp"`
}

// ManagedPublishOutcomeClass is the publisher decision represented by an
// outcome fixture.
type ManagedPublishOutcomeClass string

// Managed publish outcome classes.
const (
	ManagedPublishAccepted  ManagedPublishOutcomeClass = "accepted"
	ManagedPublishPermanent ManagedPublishOutcomeClass = "permanent"
	ManagedPublishRetryable ManagedPublishOutcomeClass = "retryable"
	ManagedPublishAmbiguous ManagedPublishOutcomeClass = "ambiguous"
)

// ManagedPublishOutcomeAction is the action a publisher takes for an outcome.
type ManagedPublishOutcomeAction string

// Managed publish outcome actions.
const (
	ManagedPublishComplete ManagedPublishOutcomeAction = "complete"
	ManagedPublishFail     ManagedPublishOutcomeAction = "fail"
	ManagedPublishRetry    ManagedPublishOutcomeAction = "retry"
)

// ManagedPublishTransportError is an acceptance-ambiguous transport failure
// represented by the portable outcome corpus.
type ManagedPublishTransportError string

// Managed publish transport failures.
const (
	ManagedPublishTimeout                 ManagedPublishTransportError = "timeout"
	ManagedPublishReset                   ManagedPublishTransportError = "reset"
	ManagedPublishClientCanceledAfterSend ManagedPublishTransportError = "client-canceled-after-send"
)

// ManagedPublishHTTPResult is the HTTP input represented by an outcome fixture.
type ManagedPublishHTTPResult struct {
	// Status is the HTTP status code of the publish response (100–599).
	Status int `json:"status"`
	// Headers holds response headers relevant to the decision (e.g. Retry-After).
	Headers map[string]string `json:"headers,omitempty"`
	// Body is the raw response body: a PublishRef on HTTP 200, an
	// EventServerError on structured rejections.
	Body json.RawMessage `json:"body"`
}

// ManagedPublishOutcomeInput contains exactly one HTTP result or transport
// failure. Transport failures are acceptance-ambiguous by definition.
type ManagedPublishOutcomeInput struct {
	// HTTP is the HTTP response received, when the request completed at the
	// HTTP layer.
	HTTP *ManagedPublishHTTPResult `json:"http,omitempty"`
	// TransportError is the transport failure observed instead of an HTTP
	// response; such failures are acceptance-ambiguous by definition.
	TransportError ManagedPublishTransportError `json:"transportError,omitempty"`
}

// ManagedPublishOutcomeExpectation pins the portable publisher decision for an
// outcome. Retried requests must stay on the same Event Server route and reuse
// the exact request bytes.
type ManagedPublishOutcomeExpectation struct {
	// Class is the outcome classification the publisher must reach.
	Class ManagedPublishOutcomeClass `json:"class"`
	// Action is the action the publisher must take (complete, fail, or retry).
	Action ManagedPublishOutcomeAction `json:"action"`
	// SameRoute requires the retry to stay on the same Event Server route.
	SameRoute bool `json:"sameRoute,omitempty"`
	// SameRequestBytes requires the retry to reuse the exact request bytes.
	SameRequestBytes bool `json:"sameRequestBytes,omitempty"`
	// HonorRetryAfter requires the publisher to honor the response's
	// Retry-After header before retrying.
	HonorRetryAfter bool `json:"honorRetryAfter,omitempty"`
}

// ManagedPublishOutcomeFixture is one portable publisher outcome case.
type ManagedPublishOutcomeFixture struct {
	// Name uniquely identifies the fixture within the corpus.
	Name string `json:"name"`
	// Request holds the request fields an accepted response must match.
	Request ManagedPublishRequestRef `json:"request"`
	// Input is the HTTP result or transport failure the publisher observed.
	Input ManagedPublishOutcomeInput `json:"input"`
	// Expected pins the decision the publisher must reach for Input.
	Expected ManagedPublishOutcomeExpectation `json:"expected"`
}

// ManagedPublishRequestRef contains the request fields an accepted response
// must match.
type ManagedPublishRequestRef struct {
	// ID is the stable request ID the accepted response must echo.
	ID string `json:"id"`
	// Topic is the logical topic the accepted response must echo.
	Topic string `json:"topic"`
}

var managedPublishAllowedFields = map[string]bool{
	"protocol":     true,
	"type":         true,
	"id":           true,
	"topic":        true,
	"payload":      true,
	"key":          true,
	"dedupeKey":    true,
	"topicVersion": true,
	"attributes":   true,
	"traceId":      true,
}

var managedPublishReservedAttributes = map[string]bool{
	"workspace_id":           true,
	"environment":            true,
	"workload":               true,
	"service":                true,
	"channel":                true,
	"topology_generation_id": true,
	"traceparent":            true,
	"tracestate":             true,
}

//go:embed fixtures/managed-publish/v1
var managedPublishCorpus embed.FS

// ManagedPublishV1Fixtures returns the read-only canonical fixture corpus with
// valid, invalid, and outcomes at its root. Go and external conformance runners
// can consume these exact embedded bytes without maintaining a copy.
func ManagedPublishV1Fixtures() fs.FS {
	fixtures, err := fs.Sub(managedPublishCorpus, ManagedPublishFixtureRoot)
	if err != nil {
		panic("events: embedded managed-publish v1 fixture root is missing")
	}
	return fixtures
}

// ReadManagedPublishV1Fixture reads one path relative to the managed-publish v1
// corpus root.
func ReadManagedPublishV1Fixture(name string) ([]byte, error) {
	return fs.ReadFile(ManagedPublishV1Fixtures(), name)
}

// ParseAndValidateManagedPublishFrame strictly decodes the managed-workload
// profile of EventServerFrame. Only managed request fields are accepted;
// channel and other frame-profile fields must be omitted, even when empty.
func ParseAndValidateManagedPublishFrame(data []byte) (*EventServerFrame, []diag.Diagnostic) {
	frame, diags := ParseEventServerFrameStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to inspect managed publish frame: %v", err),
		}
	}
	for field := range fields {
		if !managedPublishAllowedFields[field] {
			diags = append(diags, diag.Errorf("forbidden-field", field, "managed publish requests must omit %q", field))
		}
	}

	return frame, append(diags, ValidateManagedPublishFrame(frame)...)
}

// ValidateManagedPublishFrame checks the stricter managed-workload profile over
// the existing EventServerFrame publish shape.
func ValidateManagedPublishFrame(frame *EventServerFrame) []diag.Diagnostic {
	if frame == nil {
		return []diag.Diagnostic{diag.Errorf("nil-frame", "", "managed publish frame is nil")}
	}

	var diags []diag.Diagnostic
	if frame.Protocol != Protocol {
		diags = append(diags, diag.Errorf("invalid-protocol", "protocol", "expected %q, got %q", Protocol, frame.Protocol))
	}
	if frame.Type != FramePublish {
		diags = append(diags, diag.Errorf("invalid-frame-type", "type", "managed publish frame type must be %q", FramePublish))
	}
	if strings.TrimSpace(frame.ID) == "" {
		diags = append(diags, diag.Errorf("required-field", "id", "managed publish frame requires a non-empty stable id"))
	}
	if strings.TrimSpace(frame.DedupeKey) == "" {
		diags = append(diags, diag.Errorf("required-field", "dedupeKey", "managed publish frame requires a non-empty stable dedupeKey"))
	}
	if strings.TrimSpace(frame.Topic) == "" {
		diags = append(diags, diag.Errorf("required-field", "topic", "managed publish frame requires a non-empty logical topic"))
	} else if isPhysicalProviderTopic(frame.Topic) {
		diags = append(diags, diag.Errorf("invalid-topic", "topic", "managed publish topic must be logical, not a provider resource name"))
	}
	if strings.TrimSpace(frame.TopicVersion) == "" {
		diags = append(diags, diag.Errorf("required-field", "topicVersion", "managed publish frame requires a non-empty topicVersion"))
	}
	if len(frame.Payload) == 0 {
		diags = append(diags, diag.Errorf("required-field", "payload", "managed publish frame requires a JSON payload"))
	} else if !json.Valid(frame.Payload) {
		diags = append(diags, diag.Errorf("invalid-json", "payload", "managed publish payload must be valid JSON"))
	}
	if frame.Channel != "" {
		diags = append(diags, diag.Errorf("forbidden-field", "channel", "managed publish requests must omit channel; the Event Server stamps it"))
	}
	if frame.From != "" || frame.Timestamp != "" || frame.Token != "" || len(frame.Headers) != 0 || frame.Message != "" || frame.Error != "" {
		diags = append(diags, diag.Errorf("forbidden-field", "", "managed publish request contains fields owned by another EventServerFrame profile"))
	}
	for attribute := range frame.Attributes {
		if IsManagedPublishReservedAttribute(attribute) {
			diags = append(diags, diag.Errorf("reserved-attribute", "attributes."+attribute, "managed publish callers must not set reserved attribute %q", attribute))
		}
	}
	return diags
}

// IsManagedPublishReservedAttribute reports whether an attribute is stamped by
// managed admission or reserved for authenticated/routing metadata.
func IsManagedPublishReservedAttribute(attribute string) bool {
	return managedPublishReservedAttributes[attribute] ||
		strings.HasPrefix(attribute, "auth.") ||
		strings.HasPrefix(attribute, "putnami.")
}

// ValidateManagedPublishRetry proves that a retry is a valid managed request
// and reuses the exact request bytes. Stable identity alone is insufficient:
// key, topic version, attributes, and payload bytes must not mutate either.
func ValidateManagedPublishRetry(first, retry []byte) []diag.Diagnostic {
	_, firstDiags := ParseAndValidateManagedPublishFrame(first)
	_, retryDiags := ParseAndValidateManagedPublishFrame(retry)
	diags := make([]diag.Diagnostic, 0, len(firstDiags)+len(retryDiags)+1)
	diags = append(diags, firstDiags...)
	diags = append(diags, retryDiags...)
	if !bytes.Equal(first, retry) {
		diags = append(diags, diag.Errorf("retry-request-mutated", "", "managed publish retry must reuse the exact request bytes"))
	}
	return diags
}

// ParsePublishRefStrict decodes a successful publish response and rejects
// unknown fields.
func ParsePublishRefStrict(data []byte) (*PublishRef, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var ref PublishRef
	if err := dec.Decode(&ref); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse publish ref: %v", err),
		}
	}
	return &ref, nil
}

// ParseAndValidatePublishRef strictly decodes and validates a successful
// publish response.
func ParseAndValidatePublishRef(data []byte) (*PublishRef, []diag.Diagnostic) {
	ref, diags := ParsePublishRefStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return ref, append(diags, ValidatePublishRef(ref)...)
}

// ValidatePublishRef checks the canonical successful publish response.
func ValidatePublishRef(ref *PublishRef) []diag.Diagnostic {
	if ref == nil {
		return []diag.Diagnostic{diag.Errorf("nil-publish-ref", "", "publish ref is nil")}
	}

	var diags []diag.Diagnostic
	if ref.Protocol != Protocol {
		diags = append(diags, diag.Errorf("invalid-protocol", "protocol", "expected %q, got %q", Protocol, ref.Protocol))
	}
	if strings.TrimSpace(ref.ID) == "" {
		diags = append(diags, diag.Errorf("required-field", "id", "publish ref requires id"))
	}
	if strings.TrimSpace(ref.Topic) == "" {
		diags = append(diags, diag.Errorf("required-field", "topic", "publish ref requires topic"))
	}
	if ref.Timestamp == "" {
		diags = append(diags, diag.Errorf("required-field", "timestamp", "publish ref requires timestamp"))
	} else if _, err := time.Parse(time.RFC3339Nano, ref.Timestamp); err != nil {
		diags = append(diags, diag.Errorf("invalid-timestamp", "timestamp", "timestamp must be RFC3339Nano: %v", err))
	}
	return diags
}

// ParseAndValidateManagedPublishOutcomeFixture strictly decodes one outcome
// fixture and validates that its expected decision matches the managed contract.
func ParseAndValidateManagedPublishOutcomeFixture(data []byte) (*ManagedPublishOutcomeFixture, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var fixture ManagedPublishOutcomeFixture
	if err := dec.Decode(&fixture); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse managed publish outcome fixture: %v", err),
		}
	}
	return &fixture, ValidateManagedPublishOutcomeFixture(&fixture)
}

// ValidateManagedPublishOutcomeFixture checks a portable publisher outcome
// fixture without implementing a transport or making a network request.
func ValidateManagedPublishOutcomeFixture(fixture *ManagedPublishOutcomeFixture) []diag.Diagnostic {
	if fixture == nil {
		return []diag.Diagnostic{diag.Errorf("nil-outcome", "", "managed publish outcome fixture is nil")}
	}

	var diags []diag.Diagnostic
	if strings.TrimSpace(fixture.Name) == "" {
		diags = append(diags, diag.Errorf("required-field", "name", "outcome fixture requires name"))
	}
	if strings.TrimSpace(fixture.Request.ID) == "" {
		diags = append(diags, diag.Errorf("required-field", "request.id", "outcome fixture requires request id"))
	}
	if strings.TrimSpace(fixture.Request.Topic) == "" {
		diags = append(diags, diag.Errorf("required-field", "request.topic", "outcome fixture requires request topic"))
	}
	hasHTTP := fixture.Input.HTTP != nil
	hasTransportError := fixture.Input.TransportError != ""
	if hasHTTP == hasTransportError {
		diags = append(diags, diag.Errorf("invalid-outcome-input", "input", "outcome fixture requires exactly one of http or transportError"))
	}
	if hasTransportError {
		switch fixture.Input.TransportError {
		case ManagedPublishTimeout, ManagedPublishReset, ManagedPublishClientCanceledAfterSend:
		default:
			diags = append(diags, diag.Errorf("invalid-enum", "input.transportError", "unknown managed publish transport error %q", fixture.Input.TransportError))
		}
	}
	if hasHTTP && (fixture.Input.HTTP.Status < 100 || fixture.Input.HTTP.Status > 599) {
		diags = append(diags, diag.Errorf("invalid-status", "input.http.status", "HTTP status must be between 100 and 599"))
	}

	expected := fixture.Expected
	switch expected.Class {
	case ManagedPublishAccepted:
		diags = append(diags, validateAcceptedOutcome(fixture)...)
	case ManagedPublishPermanent:
		diags = append(diags, validateStructuredRejectionOutcome(fixture, false)...)
	case ManagedPublishRetryable:
		diags = append(diags, validateStructuredRejectionOutcome(fixture, true)...)
	case ManagedPublishAmbiguous:
		diags = append(diags, validateAmbiguousOutcome(fixture)...)
	default:
		diags = append(diags, diag.Errorf("invalid-enum", "expected.class", "unknown managed publish outcome class %q", expected.Class))
	}

	if expected.Action == ManagedPublishRetry {
		if !expected.SameRoute {
			diags = append(diags, diag.Errorf("unsafe-retry", "expected.sameRoute", "managed publish retries must stay on the same Event Server route"))
		}
		if !expected.SameRequestBytes {
			diags = append(diags, diag.Errorf("unsafe-retry", "expected.sameRequestBytes", "managed publish retries must reuse the exact request bytes"))
		}
	} else if expected.SameRoute || expected.SameRequestBytes || expected.HonorRetryAfter {
		diags = append(diags, diag.Errorf("invalid-expectation", "expected", "non-retry outcomes must not declare retry behavior"))
	}
	return diags
}

func validateAmbiguousOutcome(fixture *ManagedPublishOutcomeFixture) []diag.Diagnostic {
	if fixture.Expected.Action != ManagedPublishRetry {
		return []diag.Diagnostic{diag.Errorf("invalid-action", "expected.action", "ambiguous outcome action must be retry")}
	}
	if fixture.Input.TransportError != "" || fixture.Input.HTTP == nil {
		return nil
	}

	http := fixture.Input.HTTP
	if http.Status == 200 {
		ref, diags := ParseAndValidatePublishRef(http.Body)
		if !diag.HasErrors(diags) && ref.ID == fixture.Request.ID && ref.Topic == fixture.Request.Topic {
			return []diag.Diagnostic{diag.Errorf("invalid-outcome-class", "expected.class", "matching HTTP 200 publish ref must be accepted")}
		}
	}
	if isKnownStructuredRejection(http) {
		return []diag.Diagnostic{diag.Errorf("invalid-outcome-class", "expected.class", "structured HTTP %d rejection must use its defined outcome class", http.Status)}
	}
	return nil
}

func isKnownStructuredRejection(result *ManagedPublishHTTPResult) bool {
	errorBody, diags := ParseAndValidateEventServerError(result.Body)
	if diag.HasErrors(diags) || !hasJSONField(result.Body, "retryable") {
		return false
	}
	switch result.Status {
	case 400, 401, 403:
		return !errorBody.Retryable
	case 429, 503:
		return errorBody.Retryable
	default:
		return false
	}
}

func validateAcceptedOutcome(fixture *ManagedPublishOutcomeFixture) []diag.Diagnostic {
	if fixture.Input.HTTP == nil || fixture.Input.HTTP.Status != 200 {
		return []diag.Diagnostic{diag.Errorf("invalid-status", "input.http.status", "accepted outcome requires HTTP 200")}
	}
	if fixture.Expected.Action != ManagedPublishComplete {
		return []diag.Diagnostic{diag.Errorf("invalid-action", "expected.action", "accepted outcome action must be complete")}
	}

	ref, diags := ParseAndValidatePublishRef(fixture.Input.HTTP.Body)
	if diag.HasErrors(diags) {
		return diags
	}
	if ref.ID != fixture.Request.ID {
		diags = append(diags, diag.Errorf("response-mismatch", "input.http.body.id", "accepted response id must match request id"))
	}
	if ref.Topic != fixture.Request.Topic {
		diags = append(diags, diag.Errorf("response-mismatch", "input.http.body.topic", "accepted response topic must match request topic"))
	}
	return diags
}

func validateStructuredRejectionOutcome(fixture *ManagedPublishOutcomeFixture, retryable bool) []diag.Diagnostic {
	if fixture.Input.HTTP == nil {
		return []diag.Diagnostic{diag.Errorf("invalid-outcome-input", "input.http", "structured rejection requires an HTTP response")}
	}
	status := fixture.Input.HTTP.Status
	if retryable {
		if status != 429 && status != 503 {
			return []diag.Diagnostic{diag.Errorf("invalid-status", "input.http.status", "retryable rejection requires HTTP 429 or 503")}
		}
		if fixture.Expected.Action != ManagedPublishRetry {
			return []diag.Diagnostic{diag.Errorf("invalid-action", "expected.action", "retryable rejection action must be retry")}
		}
	} else {
		if status != 400 && status != 401 && status != 403 {
			return []diag.Diagnostic{diag.Errorf("invalid-status", "input.http.status", "permanent rejection requires HTTP 400, 401, or 403")}
		}
		if fixture.Expected.Action != ManagedPublishFail {
			return []diag.Diagnostic{diag.Errorf("invalid-action", "expected.action", "permanent rejection action must be fail")}
		}
	}

	errorBody, diags := ParseAndValidateEventServerError(fixture.Input.HTTP.Body)
	if diag.HasErrors(diags) {
		return diags
	}
	if !hasJSONField(fixture.Input.HTTP.Body, "retryable") {
		diags = append(diags, diag.Errorf("required-field", "input.http.body.retryable", "structured managed publish rejection must explicitly declare retryable"))
	}
	if errorBody.Retryable != retryable {
		diags = append(diags, diag.Errorf("retryability-mismatch", "input.http.body.retryable", "structured rejection retryable = %t, want %t", errorBody.Retryable, retryable))
	}
	if retryable && hasHeader(fixture.Input.HTTP.Headers, "Retry-After") && !fixture.Expected.HonorRetryAfter {
		diags = append(diags, diag.Errorf("unsafe-retry", "expected.honorRetryAfter", "publisher must honor Retry-After when supplied"))
	}
	return diags
}

func hasJSONField(data []byte, field string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return false
	}
	_, ok := object[field]
	return ok
}

func hasHeader(headers map[string]string, name string) bool {
	for header := range headers {
		if strings.EqualFold(header, name) {
			return true
		}
	}
	return false
}

func isPhysicalProviderTopic(topic string) bool {
	return strings.HasPrefix(topic, "projects/") && strings.Contains(topic, "/topics/")
}
