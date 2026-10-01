package events

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/errors"
	protoevents "go.putnami.dev/protocol/events"
)

const (
	// TransportKindEventServer selects the canonical managed Event Server HTTP
	// publisher from events.transport.
	TransportKindEventServer = "eventserver"
	// EventServerContractVersionV1 is the managed admission/config contract
	// version. The wire protocol remains putnami.events.v1.
	EventServerContractVersionV1 = protoevents.ManagedPublishContractVersion

	defaultEventServerMaxRequestBytes  int64 = 1 << 20
	defaultEventServerMaxResponseBytes int64 = 64 << 10
)

// Event Server publish error codes. Callers that need outcome-aware handling
// should errors.As to *EventServerPublishError and inspect Outcome.
const (
	CodeEventsEventServerConfig      errors.Code = "events.eventserver.config"
	CodeEventsEventServerCredential  errors.Code = "events.eventserver.credential"
	CodeEventsEventServerPermanent   errors.Code = "events.eventserver.permanent"
	CodeEventsEventServerRetryable   errors.Code = "events.eventserver.retryable"
	CodeEventsEventServerAmbiguous   errors.Code = "events.eventserver.ambiguous"
	CodeEventsEventServerUnsupported errors.Code = "events.eventserver.unsupported"
)

// EventServerPublishOutcome classifies the result of a managed publish.
type EventServerPublishOutcome string

// Managed publish outcomes.
const (
	EventServerPublishPermanent EventServerPublishOutcome = "permanent"
	EventServerPublishRetryable EventServerPublishOutcome = "retryable"
	EventServerPublishAmbiguous EventServerPublishOutcome = "ambiguous"
)

// EventServerPublishError is the typed failure returned by EventServerTransport.
// Ambiguous means the server may have accepted the stable event identity; a
// caller must never fall back or dual-publish it through another transport.
type EventServerPublishError struct {
	Outcome      EventServerPublishOutcome
	StatusCode   int
	ProtocolCode protoevents.ErrorCode
	cause        error
}

func (e *EventServerPublishError) Error() string {
	if e == nil || e.cause == nil {
		return "events.eventserver: publish failed"
	}
	return e.cause.Error()
}

// Unwrap exposes the framework structured error for errors.Is/errors.As.
func (e *EventServerPublishError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// EventServerPublishOutcomeOf extracts an Event Server publish outcome.
func EventServerPublishOutcomeOf(err error) (EventServerPublishOutcome, bool) {
	var publishErr *EventServerPublishError
	if !stderrors.As(err, &publishErr) {
		return "", false
	}
	return publishErr.Outcome, true
}

// CredentialSource returns a bearer credential scoped to audience. Provider-
// specific token acquisition belongs in the registering cloud integration, not
// this framework package.
type CredentialSource interface {
	Token(context.Context, string) (string, error)
}

// CredentialSourceFunc adapts a function to CredentialSource.
type CredentialSourceFunc func(context.Context, string) (string, error)

// Token implements CredentialSource.
func (f CredentialSourceFunc) Token(ctx context.Context, audience string) (string, error) {
	return f(ctx, audience)
}

// W3CTraceContext carries the HTTP trace headers used by the managed publish
// boundary. These are transport headers, never envelope authority attributes.
type W3CTraceContext struct {
	Traceparent string
	Tracestate  string
}

// TraceContextSource extracts W3C trace context from a publish context.
type TraceContextSource interface {
	TraceContext(context.Context) W3CTraceContext
}

type w3cTraceContextKey struct{}

// WithW3CTraceContext attaches W3C headers to a context for an Event Server
// publish. Integrations with a tracing SDK may instead set TraceContextSource.
func WithW3CTraceContext(ctx context.Context, traceparent, tracestate string) context.Context {
	return context.WithValue(ctx, w3cTraceContextKey{}, W3CTraceContext{
		Traceparent: traceparent,
		Tracestate:  tracestate,
	})
}

// EventServerBinding is the resolved events.eventServer config. Workspace and
// topology fields are local compatibility hints only: the publisher never sends
// them as authority. Event Server resolves and stamps authoritative identity.
type EventServerBinding struct {
	ContractVersion      int    `json:"contractVersion"`
	Endpoint             string `json:"endpoint"`
	Audience             string `json:"audience"`
	Protocol             string `json:"protocol"`
	WorkspaceID          string `json:"workspaceId"`
	Environment          string `json:"environment"`
	Workload             string `json:"workload"`
	TopologyGenerationID string `json:"topologyGenerationId"`

	CredentialSource   CredentialSource   `json:"-"`
	TraceContextSource TraceContextSource `json:"-"`
}

// EventServerTransportConfig configures the canonical HTTP publisher.
type EventServerTransportConfig struct {
	ContractVersion    int
	Endpoint           string
	Audience           string
	Protocol           string
	CredentialSource   CredentialSource
	TraceContextSource TraceContextSource
	HTTPClient         interface {
		Do(*http.Request) (*http.Response, error)
	}
	Timeout          time.Duration
	MaxAttempts      int
	BaseBackoff      time.Duration
	MaxBackoff       time.Duration
	MaxRequestBytes  int64
	MaxResponseBytes int64
}

// EventServerTransportConfigFromBinding converts a resolved binding into the
// default transport config while deliberately ignoring caller authority hints.
func EventServerTransportConfigFromBinding(binding EventServerBinding) EventServerTransportConfig {
	return EventServerTransportConfig{
		ContractVersion:    binding.ContractVersion,
		Endpoint:           binding.Endpoint,
		Audience:           binding.Audience,
		Protocol:           binding.Protocol,
		CredentialSource:   binding.CredentialSource,
		TraceContextSource: binding.TraceContextSource,
	}
}

// EventServerTransport publishes canonical managed frames to Event Server. It
// intentionally has no provider fallback and no subscription implementation.
type EventServerTransport struct {
	endpoint     string
	audience     string
	protocol     string
	credentials  CredentialSource
	traceContext TraceContextSource
	client       interface {
		Do(*http.Request) (*http.Response, error)
	}
	timeout          time.Duration
	maxAttempts      int
	baseBackoff      time.Duration
	maxBackoff       time.Duration
	maxRequestBytes  int64
	maxResponseBytes int64
}

// NewEventServerTransport creates a credential-pluggable managed publisher.
func NewEventServerTransport(config EventServerTransportConfig) (*EventServerTransport, error) {
	endpoint, err := validateEventServerConfig(config)
	if err != nil {
		return nil, err
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	maxAttempts := config.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	baseBackoff := config.BaseBackoff
	if baseBackoff <= 0 {
		baseBackoff = 100 * time.Millisecond
	}
	maxBackoff := config.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 2 * time.Second
	}
	maxRequestBytes := config.MaxRequestBytes
	if maxRequestBytes <= 0 {
		maxRequestBytes = defaultEventServerMaxRequestBytes
	}
	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultEventServerMaxResponseBytes
	}
	return &EventServerTransport{
		endpoint:         endpoint,
		audience:         config.Audience,
		protocol:         config.Protocol,
		credentials:      config.CredentialSource,
		traceContext:     config.TraceContextSource,
		client:           client,
		timeout:          timeout,
		maxAttempts:      maxAttempts,
		baseBackoff:      baseBackoff,
		maxBackoff:       maxBackoff,
		maxRequestBytes:  maxRequestBytes,
		maxResponseBytes: maxResponseBytes,
	}, nil
}

func validateEventServerConfig(config EventServerTransportConfig) (string, error) {
	if config.ContractVersion != EventServerContractVersionV1 {
		return "", errors.New(CodeEventsEventServerConfig, "unsupported Event Server contract version",
			errors.Int("contract_version", config.ContractVersion))
	}
	if config.Protocol != protoevents.Protocol {
		return "", errors.New(CodeEventsEventServerConfig, "unsupported Event Server wire protocol",
			errors.String("protocol", config.Protocol))
	}
	if config.Audience == "" {
		return "", errors.New(CodeEventsEventServerConfig, "Event Server audience is required")
	}
	if config.CredentialSource == nil {
		return "", errors.New(CodeEventsEventServerConfig, "Event Server credential source is required")
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New(CodeEventsEventServerConfig, "Event Server endpoint must be an absolute URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New(CodeEventsEventServerConfig, "Event Server endpoint must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New(CodeEventsEventServerConfig, "Event Server endpoint must be an origin without credentials, path, query, or fragment")
	}
	parsed.Path = protoevents.DefaultEventServerPublishPath
	return parsed.String(), nil
}

type eventServerPublishRequest struct {
	Protocol     string                `json:"protocol"`
	Type         protoevents.FrameType `json:"type"`
	ID           string                `json:"id"`
	Topic        string                `json:"topic"`
	Payload      json.RawMessage       `json:"payload"`
	Key          string                `json:"key,omitempty"`
	DedupeKey    string                `json:"dedupeKey"`
	TopicVersion string                `json:"topicVersion"`
	Attributes   map[string]string     `json:"attributes,omitempty"`
	TraceID      string                `json:"traceId,omitempty"`
}

// Publish sends one canonical managed frame. The body and identity are built
// once and reused byte-for-byte across every internal retry.
func (t *EventServerTransport) Publish(ctx context.Context, env Envelope) error {
	publishCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	body, request, err := t.buildRequest(env)
	if err != nil {
		return err
	}
	token, credentialErr := t.credentials.Token(publishCtx, t.audience)
	if credentialErr != nil {
		// Never wrap a provider error: token sources occasionally include response
		// material in errors, and credentials must not reach logs.
		return newEventServerPublishError(EventServerPublishRetryable, 0, "", CodeEventsEventServerCredential,
			"Event Server credential source failed")
	}
	if strings.TrimSpace(token) == "" || token != strings.TrimSpace(token) || strings.ContainsAny(token, "\r\n") {
		return newEventServerPublishError(EventServerPublishPermanent, 0, "", CodeEventsEventServerCredential,
			"Event Server credential source returned an invalid token")
	}
	traceContext, err := t.traceHeaders(publishCtx)
	if err != nil {
		return err
	}

	var lastErr *EventServerPublishError
	var retryAfter string
	for attempt := 1; attempt <= t.maxAttempts; attempt++ {
		if attempt > 1 {
			delay := t.retryDelay(attempt-1, retryAfter)
			if err := waitForEventServerRetry(publishCtx, delay); err != nil {
				if lastErr != nil {
					return lastErr
				}
				return newEventServerPublishError(EventServerPublishAmbiguous, 0, "", CodeEventsEventServerAmbiguous,
					"Event Server publish deadline elapsed")
			}
		}

		result, nextRetryAfter := t.publishAttempt(publishCtx, body, token, traceContext, request)
		if result == nil {
			return nil
		}
		lastErr = result
		retryAfter = nextRetryAfter
		if result.Outcome == EventServerPublishPermanent || publishCtx.Err() != nil {
			return result
		}
	}
	return lastErr
}

func (t *EventServerTransport) buildRequest(env Envelope) ([]byte, eventServerPublishRequest, error) {
	if env.Protocol != "" && env.Protocol != t.protocol {
		return nil, eventServerPublishRequest{}, newEventServerPublishError(EventServerPublishPermanent, 0,
			protoevents.ErrorProtocolMismatch, CodeEventsEventServerPermanent, "managed publish protocol mismatch")
	}
	if env.Channel != "" {
		return nil, eventServerPublishRequest{}, newEventServerPublishError(EventServerPublishPermanent, 0,
			protoevents.ErrorInvalidFrame, CodeEventsEventServerPermanent, "managed publish channel is server authority")
	}
	if env.Topic == "" || env.TopicVersion == "" {
		return nil, eventServerPublishRequest{}, newEventServerPublishError(EventServerPublishPermanent, 0,
			protoevents.ErrorInvalidFrame, CodeEventsEventServerPermanent, "managed publish requires topic and topicVersion")
	}
	for key := range env.Attributes {
		if protoevents.IsManagedPublishReservedAttribute(key) {
			return nil, eventServerPublishRequest{}, newEventServerPublishError(EventServerPublishPermanent, 0,
				protoevents.ErrorInvalidFrame, CodeEventsEventServerPermanent, "managed publish contains a reserved authority attribute")
		}
	}
	payload, err := json.Marshal(env.Payload)
	if err != nil || !json.Valid(payload) {
		return nil, eventServerPublishRequest{}, newEventServerPublishError(EventServerPublishPermanent, 0,
			protoevents.ErrorInvalidEnvelope, CodeEventsEventServerPermanent, "managed publish payload is not valid JSON")
	}
	if env.ID == "" {
		env.ID = generateID()
	}
	if env.DedupeKey == "" {
		env.DedupeKey = env.ID
	}
	request := eventServerPublishRequest{
		Protocol:     t.protocol,
		Type:         protoevents.FramePublish,
		ID:           env.ID,
		Topic:        env.Topic,
		Payload:      payload,
		Key:          env.Key,
		DedupeKey:    env.DedupeKey,
		TopicVersion: env.TopicVersion,
		Attributes:   env.Attributes,
		TraceID:      env.TraceID,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, eventServerPublishRequest{}, newEventServerPublishError(EventServerPublishPermanent, 0,
			protoevents.ErrorInvalidEnvelope, CodeEventsEventServerPermanent, "managed publish frame could not be encoded")
	}
	if int64(len(body)) > t.maxRequestBytes {
		return nil, eventServerPublishRequest{}, newEventServerPublishError(EventServerPublishPermanent, 0,
			protoevents.ErrorPayloadTooLarge, CodeEventsEventServerPermanent, "managed publish frame exceeds the configured size limit")
	}
	if _, diags := protoevents.ParseAndValidateManagedPublishFrame(body); len(diags) != 0 {
		return nil, eventServerPublishRequest{}, newEventServerPublishError(EventServerPublishPermanent, 0,
			protoevents.ErrorInvalidFrame, CodeEventsEventServerPermanent, "managed publish frame violates the canonical profile")
	}
	return body, request, nil
}

func (t *EventServerTransport) traceHeaders(ctx context.Context) (W3CTraceContext, error) {
	var trace W3CTraceContext
	if t.traceContext != nil {
		trace = t.traceContext.TraceContext(ctx)
	} else if value, ok := ctx.Value(w3cTraceContextKey{}).(W3CTraceContext); ok {
		trace = value
	}
	if invalidEventServerHeader(trace.Traceparent, 512) || invalidEventServerHeader(trace.Tracestate, 4096) {
		return W3CTraceContext{}, newEventServerPublishError(EventServerPublishPermanent, 0,
			protoevents.ErrorInvalidFrame, CodeEventsEventServerPermanent, "invalid W3C trace context")
	}
	return trace, nil
}

func invalidEventServerHeader(value string, maxLen int) bool {
	return len(value) > maxLen || strings.ContainsAny(value, "\r\n")
}

func (t *EventServerTransport) publishAttempt(
	ctx context.Context,
	body []byte,
	token string,
	trace W3CTraceContext,
	request eventServerPublishRequest,
) (*EventServerPublishError, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return newEventServerPublishError(EventServerPublishPermanent, 0, "", CodeEventsEventServerPermanent,
			"Event Server publish request could not be created"), ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if trace.Traceparent != "" {
		req.Header.Set("traceparent", trace.Traceparent)
	}
	if trace.Tracestate != "" {
		req.Header.Set("tracestate", trace.Tracestate)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return newEventServerPublishError(EventServerPublishAmbiguous, 0, "", CodeEventsEventServerAmbiguous,
			"Event Server publish acceptance is unknown"), ""
	}
	responseBody, tooLarge, readErr := readEventServerResponse(resp.Body, t.maxResponseBytes)
	if closeErr := resp.Body.Close(); readErr == nil {
		readErr = closeErr
	}
	if readErr != nil || tooLarge {
		outcome := EventServerPublishAmbiguous
		code := CodeEventsEventServerAmbiguous
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			outcome = EventServerPublishPermanent
			code = CodeEventsEventServerPermanent
		}
		return newEventServerPublishError(outcome, resp.StatusCode, "", code,
			"Event Server response could not be validated"), resp.Header.Get("Retry-After")
	}

	if resp.StatusCode == http.StatusOK {
		accepted, diags := protoevents.ParseAndValidatePublishRef(responseBody)
		if len(diags) != 0 || accepted == nil ||
			accepted.Protocol != t.protocol || accepted.ID != request.ID || accepted.Topic != request.Topic || accepted.Timestamp == "" {
			return newEventServerPublishError(EventServerPublishAmbiguous, resp.StatusCode, "", CodeEventsEventServerAmbiguous,
				"Event Server returned a malformed or mismatched acceptance"), ""
		}
		return nil, ""
	}

	serverError, structured, explicitRetryable := parseEventServerError(responseBody)
	protocolCode := protoevents.ErrorCode("")
	if structured {
		protocolCode = serverError.Code
	}
	switch {
	case structured && explicitRetryable && !serverError.Retryable &&
		(resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden):
		return newEventServerPublishError(EventServerPublishPermanent, resp.StatusCode, protocolCode,
			CodeEventsEventServerPermanent, "Event Server permanently rejected the publish"), ""
	case structured && explicitRetryable && serverError.Retryable &&
		(resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable):
		return newEventServerPublishError(EventServerPublishRetryable, resp.StatusCode, protocolCode,
			CodeEventsEventServerRetryable, "Event Server asked the publisher to retry"), resp.Header.Get("Retry-After")
	default:
		return newEventServerPublishError(EventServerPublishAmbiguous, resp.StatusCode, protocolCode,
			CodeEventsEventServerAmbiguous, "Event Server publish acceptance is unknown"), resp.Header.Get("Retry-After")
	}
}

func readEventServerResponse(reader io.Reader, maxBytes int64) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > maxBytes {
		return nil, true, nil
	}
	return body, false, nil
}

func parseEventServerError(body []byte) (*protoevents.EventServerError, bool, bool) {
	serverError, diags := protoevents.ParseAndValidateEventServerError(body)
	if serverError == nil || len(diags) != 0 {
		return serverError, false, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return serverError, false, false
	}
	_, explicitRetryable := fields["retryable"]
	return serverError, true, explicitRetryable
}

func newEventServerPublishError(
	outcome EventServerPublishOutcome,
	status int,
	protocolCode protoevents.ErrorCode,
	code errors.Code,
	message string,
) *EventServerPublishError {
	attrs := make([]errors.Attr, 0, 2)
	if status != 0 {
		attrs = append(attrs, errors.Int("status", status))
	}
	if protocolCode != "" {
		attrs = append(attrs, errors.String("protocol_code", string(protocolCode)))
	}
	cause := errors.New(code, message, attrs...)
	switch outcome {
	case EventServerPublishRetryable:
		cause = cause.WithCategory(errors.CategoryTransient).WithRetryable(true)
	case EventServerPublishAmbiguous:
		cause = cause.WithCategory(errors.CategoryInfra)
	default:
		cause = cause.WithCategory(errors.CategoryUser)
	}
	return &EventServerPublishError{
		Outcome:      outcome,
		StatusCode:   status,
		ProtocolCode: protocolCode,
		cause:        cause,
	}
}

func (t *EventServerTransport) retryDelay(attempt int, retryAfter string) time.Duration {
	backoff := retryBackoff(attempt, t.baseBackoff, t.maxBackoff)
	if delay, ok := parseEventServerRetryAfter(retryAfter); ok {
		if delay > backoff {
			return delay
		}
	}
	return backoff
}

func parseEventServerRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := time.Until(when)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func waitForEventServerRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe fails closed: managed Event Server transport is a publisher only.
// Push delivery remains owned by the plugin's receiver route; existing direct
// pull/stream transports are unchanged.
func (t *EventServerTransport) Subscribe(*HandlerDefinition) error {
	return errors.New(CodeEventsEventServerUnsupported, "Event Server publisher transport does not support subscriptions")
}

// Start is a no-op for the stateless HTTP publisher.
func (t *EventServerTransport) Start(context.Context) error { return nil }

// Stop releases idle HTTP connections when the configured client supports it.
func (t *EventServerTransport) Stop(context.Context) error {
	if closer, ok := t.client.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	return nil
}

var _ Transport = (*EventServerTransport)(nil)
