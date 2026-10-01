package client

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

const (
	defaultTotalTimeout   = 30 * time.Second
	defaultAttemptTimeout = 30 * time.Second
	defaultMaxAttempts    = 4
	defaultBackoffBase    = 200 * time.Millisecond
	defaultBackoffMax     = 5 * time.Second
)

var defaultRetryStatuses = []int{408, 429, 502, 503, 504}

// ServiceDescriptor is the provider-owned, credential-free document contract
// embedded by a generated client.
type ServiceDescriptor struct {
	Contract   clientcontract.DocumentV1
	Schemas    map[string]clientcontract.Schema
	Operations map[string]clientcontract.OperationV1
}

// Operation is one provider-owned operation contract embedded by a generated
// method.
type Operation struct {
	ID        string
	Contract  clientcontract.OperationV1
	Request   *OperationRequest
	Successes []OperationSuccess
}

// OperationRequest is the generated request-validation subset for an
// operation. It is separate from the provider policy extension because its
// schema comes from the standard OpenAPI requestBody declaration.
type OperationRequest struct {
	Required bool               `json:"required"`
	Content  []OperationContent `json:"content"`
}

// OperationSuccess is the response-validation subset embedded beside an
// operation policy.
type OperationSuccess struct {
	Status  int                `json:"status"`
	Content []OperationContent `json:"content"`
}

// OperationContent identifies one successful media type and schema.
type OperationContent struct {
	MediaType string                 `json:"mediaType"`
	Schema    *clientcontract.Schema `json:"schema,omitempty"`
	// MaxBytes is the declared byte bound of a raw octet representation. Zero on
	// every JSON representation.
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// Streamed marks an unframed, unbuffered binary representation. Its wildcard
	// declaration accepts a concrete Content-Type supplied on the wire.
	Streamed bool `json:"streamed,omitempty"`
}

// MustServiceDescriptor parses generated source metadata. It panics only when
// generated source is corrupt; provider input is validated before emission.
func MustServiceDescriptor(source string, schemasSource ...string) ServiceDescriptor {
	document, diagnostics := clientcontract.ParseAndValidateDocument([]byte(source))
	if len(diagnostics) > 0 {
		panic("invalid generated service descriptor: " + diagnostics[0].String())
	}
	descriptor := ServiceDescriptor{Contract: *document}
	if len(schemasSource) > 0 && schemasSource[0] != "" {
		var rawSchemas map[string]json.RawMessage
		if err := json.Unmarshal([]byte(schemasSource[0]), &rawSchemas); err != nil {
			panic("invalid generated schema descriptor")
		}
		descriptor.Schemas = make(map[string]clientcontract.Schema, len(rawSchemas))
		for name, raw := range rawSchemas {
			schema, schemaDiagnostics := clientcontract.ParseAndValidateSchema(raw)
			if len(schemaDiagnostics) > 0 {
				panic("invalid generated schema descriptor: " + schemaDiagnostics[0].String())
			}
			descriptor.Schemas[name] = *schema
		}
	}
	return descriptor
}

// MustServiceDescriptorWithOperations builds a service descriptor from the
// operations already parsed by MustOperation for generated method dispatch.
// It snapshots their contracts so later mutation cannot change the inventory.
func MustServiceDescriptorWithOperations(source, schemasSource string, operations ...Operation) ServiceDescriptor {
	descriptor := MustServiceDescriptor(source, schemasSource)
	descriptor.Operations = make(map[string]clientcontract.OperationV1, len(operations))
	for _, operation := range operations {
		if strings.TrimSpace(operation.ID) == "" {
			panic("invalid generated operation identity")
		}
		if _, exists := descriptor.Operations[operation.ID]; exists {
			panic("duplicate generated operation identity: " + operation.ID)
		}
		descriptor.Operations[operation.ID] = operation.Contract
	}
	snapshot, err := snapshotServiceDescriptor(descriptor)
	if err != nil {
		panic(err)
	}
	return snapshot
}

// MustOperation parses generated operation metadata.
func MustOperation(operationID, source string, successesSource ...string) Operation {
	operation, diagnostics := clientcontract.ParseOperation([]byte(source))
	if len(diagnostics) > 0 {
		panic("invalid generated operation descriptor: " + diagnostics[0].String())
	}
	result := Operation{ID: operationID, Contract: *operation}
	if len(successesSource) > 0 && successesSource[0] != "" {
		if err := json.Unmarshal([]byte(successesSource[0]), &result.Successes); err != nil {
			panic("invalid generated operation successes")
		}
	}
	if len(successesSource) > 1 && successesSource[1] != "" {
		var request OperationRequest
		if err := json.Unmarshal([]byte(successesSource[1]), &request); err != nil {
			panic("invalid generated operation request")
		}
		result.Request = &request
	}
	if len(successesSource) > 2 {
		panic("invalid generated operation metadata")
	}
	return result
}

type serviceRuntime struct {
	descriptor ServiceDescriptor
	binding    ServiceBinding
	// baseURL is the canonical bound endpoint, the default audience of a
	// gcp-id-token credential.
	baseURL string
	// registry is the application-scoped owner of the credential cache and of
	// the stream sessions this client opens. Stream transports track their
	// session on it so stopping the application closes them.
	registry    *ServiceBindings
	credentials *credentialManager

	mu       sync.Mutex
	breakers map[string]*CircuitBreaker
	// cachePlans holds what the response cache resolved once per operation.
	cachePlans map[cachePlanKey]*cachePlan
	// endpoints is protected by mu on the original bound runtime. Endpoint
	// views point back to that owner and never mutate its deployment binding.
	endpoints     map[string]*serviceRuntime
	endpointOwner *serviceRuntime
}

// NewServiceClient resolves a generated provider contract through the typed
// service binding registry. It is the constructor used by generated DI
// registration functions.
func NewServiceClient(bindings *ServiceBindings, descriptor ServiceDescriptor) (*Client, error) {
	if bindings == nil {
		return nil, errors.New(CodeClientConfig, "service bindings are not configured")
	}
	// Reparse serialized metadata so the client owns a complete immutable
	// snapshot; mutating generator literals or caller-owned maps after
	// construction cannot change authentication or resilience policy.
	var err error
	descriptor, err = snapshotServiceDescriptor(descriptor)
	if err != nil {
		return nil, err
	}
	binding, err := bindings.For(descriptor.Contract.Service.ID)
	if err != nil {
		return nil, err
	}
	if err := clientcontract.ValidateOperationPaths(descriptor.Operations, binding.OperationPaths); err != nil {
		return nil, errors.Newf(CodeClientConfig, "invalid operation paths: %v", err)
	}
	for _, profile := range descriptor.Contract.Credentials {
		if _, exists := binding.Headers[http.CanonicalHeaderKey(profile.Header)]; exists {
			return nil, errors.New(CodeClientConfig, "service binding header conflicts with a declared credential header")
		}
	}
	if strings.TrimSpace(binding.ClientID) == "" {
		return nil, errors.Newf(CodeClientConfig, "service %q has no client identity", descriptor.Contract.Service.ID)
	}
	endpoint, err := parseBoundURL(binding.URL, binding.AllowInsecure)
	if err != nil {
		return nil, err
	}
	httpClient := safeCredentialHTTPClient(binding.HTTPClient)
	// Context deadlines are the authority for generated calls. Leaving the
	// client-wide timeout clear also permits a later streaming transport to live
	// beyond the unary default while retaining idle/frame bounds.
	httpClient.Timeout = 0
	transport := &HTTPTransport{
		baseURL:         strings.TrimRight(endpoint.String(), "/"),
		client:          httpClient,
		maxResponseSize: defaultMaxResponseSize,
	}
	return &Client{
		transport: transport,
		config: Config{
			BaseURL:  endpoint.String(),
			ClientID: binding.ClientID,
		},
		service: &serviceRuntime{
			descriptor:  descriptor,
			binding:     binding,
			baseURL:     transport.baseURL,
			registry:    bindings,
			credentials: bindings.credentials,
			breakers:    make(map[string]*CircuitBreaker),
			cachePlans:  make(map[cachePlanKey]*cachePlan),
		},
		descriptor: &descriptor,
	}, nil
}

func snapshotServiceDescriptor(descriptor ServiceDescriptor) (ServiceDescriptor, error) {
	contractJSON, err := json.Marshal(descriptor.Contract)
	if err != nil {
		return ServiceDescriptor{}, errors.New(CodeClientConfig, "serialize generated service contract")
	}
	contractSnapshot, diagnostics := clientcontract.ParseAndValidateDocument(contractJSON)
	if len(diagnostics) > 0 {
		return ServiceDescriptor{}, errors.Newf(CodeClientConfig, "invalid generated service contract: %s", diagnostics[0].String())
	}
	descriptor.Contract = *contractSnapshot
	descriptor.Schemas = cloneSchemas(descriptor.Schemas)
	if descriptor.Operations != nil {
		raw, marshalErr := json.Marshal(descriptor.Operations)
		if marshalErr != nil {
			return ServiceDescriptor{}, errors.New(CodeClientConfig, "serialize generated operations")
		}
		var operations map[string]clientcontract.OperationV1
		if unmarshalErr := json.Unmarshal(raw, &operations); unmarshalErr != nil {
			return ServiceDescriptor{}, errors.New(CodeClientConfig, "snapshot generated operations")
		}
		descriptor.Operations = operations
	}
	return descriptor, nil
}

// WithGeneratedServiceDescriptor attaches an immutable generated schema graph
// to an explicit low-level client. It preserves response/error validation for
// tests and external integrations while leaving their transport/interceptors
// under caller control.
func WithGeneratedServiceDescriptor(base *Client, descriptor ServiceDescriptor) *Client {
	if base == nil {
		return nil
	}
	snapshot, err := snapshotServiceDescriptor(descriptor)
	if err != nil {
		panic(err)
	}
	copy := *base
	copy.descriptor = &snapshot
	return &copy
}

// BindServiceClient is the generated caller-binding entrypoint. Its distinct
// symbol makes a generated binder fail to compile against runtimes predating
// operation path routing and private registry ownership.
func BindServiceClient(binding ServiceBinding, descriptor ServiceDescriptor) (*Client, error) {
	return NewServiceClientBinding(binding, descriptor)
}

// NewServiceClientBinding constructs a generated client from an explicit
// binding. Generated New<ClientName>Binding constructors expose this path for
// config sources and other callers that run before DI exists. Normal generated
// registration resolves ServiceBindings from DI. The returned client owns its
// registry: Close releases credentials, cached responses and streams and refuses
// later calls. Reuse a binding for a sequence of pages, then close it.
func NewServiceClientBinding(binding ServiceBinding, descriptor ServiceDescriptor) (*Client, error) {
	bindings, err := newServiceBindings(ServicesOptions{
		ClientID: binding.ClientID,
		Services: map[string]ServiceBinding{descriptor.Contract.Service.ID: binding},
	})
	if err != nil {
		return nil, err
	}
	bound, err := NewServiceClient(bindings, descriptor)
	if err != nil {
		if closeErr := bindings.Close(); closeErr != nil {
			return nil, errors.NewAggregate("bind service client", []error{err, closeErr})
		}
		return nil, err
	}
	bound.ownsRegistry = true
	return bound, nil
}

type effectivePolicy struct {
	totalTimeout     time.Duration
	attemptTimeout   time.Duration
	maxResponseBytes int64
	maxAttempts      int
	retryStatuses    []int
	retryCodes       []string
	breakerThreshold int
	breakerReset     time.Duration
	backoffBase      time.Duration
	backoffMax       time.Duration
}

func resolvePolicy(document *clientcontract.ResiliencePolicy, operation *clientcontract.ResiliencePolicy, kind clientcontract.IdempotencyKind) effectivePolicy {
	policy := effectivePolicy{
		totalTimeout:     defaultTotalTimeout,
		attemptTimeout:   defaultAttemptTimeout,
		maxResponseBytes: defaultMaxResponseSize,
		maxAttempts:      defaultMaxAttempts,
		retryStatuses:    append([]int(nil), defaultRetryStatuses...),
		breakerThreshold: 5,
		breakerReset:     30 * time.Second,
	}
	if kind == clientcontract.IdempotencyNonIdempotent {
		policy.maxAttempts = 1
	}
	apply := func(value *clientcontract.ResiliencePolicy) {
		if value == nil {
			return
		}
		if value.TimeoutMs != nil {
			policy.totalTimeout = time.Duration(*value.TimeoutMs) * time.Millisecond
		}
		if value.AttemptTimeoutMs != nil {
			policy.attemptTimeout = time.Duration(*value.AttemptTimeoutMs) * time.Millisecond
		}
		if value.MaxResponseBytes != nil {
			policy.maxResponseBytes = *value.MaxResponseBytes
		}
		if value.Retry != nil {
			if value.Retry.MaxAttempts != nil {
				policy.maxAttempts = *value.Retry.MaxAttempts
			}
			if value.Retry.Statuses != nil {
				policy.retryStatuses = append([]int(nil), value.Retry.Statuses...)
			}
			if value.Retry.Codes != nil {
				policy.retryCodes = append([]string(nil), value.Retry.Codes...)
			}
		}
		if value.Circuit != nil {
			if value.Circuit.FailureThreshold != nil {
				policy.breakerThreshold = *value.Circuit.FailureThreshold
			}
			if value.Circuit.ResetTimeoutMs != nil {
				policy.breakerReset = time.Duration(*value.Circuit.ResetTimeoutMs) * time.Millisecond
			}
		}
	}
	apply(document)
	apply(operation)
	if kind == clientcontract.IdempotencyNonIdempotent {
		policy.maxAttempts = 1
	}
	if policy.attemptTimeout > policy.totalTimeout {
		policy.attemptTimeout = policy.totalTimeout
	}
	// Backoff is bounded by the budgets the ResiliencePolicy declares rather
	// than by fixed constants: a provider that declares a 500ms operation must
	// never plan a 5s wait between two of its attempts.
	policy.backoffMax = minDuration(defaultBackoffMax, policy.totalTimeout)
	policy.backoffBase = minDuration(defaultBackoffBase, policy.backoffMax)
	if policy.attemptTimeout > 0 {
		policy.backoffBase = minDuration(policy.backoffBase, policy.attemptTimeout)
	}
	return policy
}

func minDuration(left, right time.Duration) time.Duration {
	if right < left {
		return right
	}
	return left
}

// DoOperation executes one generated unary operation with provider-declared
// authentication and resilience. A non-2xx result is returned as *RemoteError;
// raw response bytes never escape through an error.
func (client *Client) DoOperation(ctx context.Context, request *Request, operation Operation) (response *Response, callErr error) {
	client, ctx, callErr = clientForEndpoint(ctx, client)
	if callErr != nil {
		return nil, callErr
	}
	if err := validateBinaryStreamOperation(operation); err != nil {
		return nil, err
	}
	return client.doOperation(ctx, request, operation)
}

func (client *Client) doOperation(ctx context.Context, request *Request, operation Operation) (response *Response, callErr error) {
	if err := validateGeneratedRequest(request, operation.Request, operationSchemas(client)); err != nil {
		return nil, err
	}
	if client.service == nil {
		// No binding means no credential manager, so nothing would authenticate
		// this request. Sending it would turn a declared-authenticated operation
		// into an anonymous call.
		if err := requireDeclaredAnonymous(operation.Contract.Security); err != nil {
			return nil, err
		}
		request = cloneRequest(request)
		request.MaxPayloadBytes = declaredBinaryResponseBytes(operation)
		request.MaxRequestBytes = declaredBinaryRequestBytes(operation)
		response, callErr = client.Do(ctx, request)
		if callErr != nil {
			closeResponseStream(response)
			return nil, callErr
		}
		if response != nil && !response.IsSuccess() {
			closeResponseStream(response)
			var schemas map[string]clientcontract.Schema
			if client.descriptor != nil {
				schemas = client.descriptor.Schemas
			}
			// No binding, so no consumer opt-in to read: the provider's prose is
			// not carried (and there are no credentials to redact it against).
			return nil, decodeRemoteError(descriptorIdentity(client, operation), response, operation.Contract.Errors, schemas, nil, false)
		}
		if response != nil && response.BodyStream != nil {
			if err := validateSuccessfulResponse(response, operation.Successes, operationSchemas(client)); err != nil {
				closeResponseStream(response)
				return nil, err
			}
		}
		return response, nil
	}
	runtime := client.service
	// A client retained past its application's stop sends nothing: the registry
	// it was bound from is closed, and so is every call through it, anonymous
	// or credentialed, REST or Connect.
	if runtime.registry != nil && runtime.registry.isClosed() {
		return nil, errClosedRegistry()
	}
	attempts := 0
	// The measurement names the wire that carries the call: the transport
	// CallOperation dispatched from the declared order, Connect included.
	protocol := string(clientcontract.TransportRESTJSON)
	if transport, err := dispatchTransport(operation, supportedUnaryTransport); err == nil {
		protocol = string(transport.Protocol)
	}
	telemetry := currentServiceTelemetry()
	if telemetry != nil {
		callInfo := ServiceCallInfo{
			ServiceID:   runtime.descriptor.Contract.Service.ID,
			OperationID: operation.ID,
			Protocol:    protocol,
		}
		var finish func(ServiceCallResult)
		ctx, finish = telemetry.StartServiceCall(ctx, callInfo)
		defer func() {
			if finish != nil {
				finish(serviceCallResult(response, callErr, attempts))
			}
		}()
	}
	if diagnostics := clientcontract.ValidateOperationForID(operation.ID, &operation.Contract, &runtime.descriptor.Contract); len(diagnostics) > 0 {
		return nil, errors.Newf(CodeClientConfig, "invalid generated operation contract: %s", diagnostics[0].String())
	}
	if operation.Contract.Stream != clientcontract.StreamUnary {
		return nil, errors.New(CodeClientConfig, "streaming operation requires a streaming transport")
	}
	var documentPolicy *clientcontract.ResiliencePolicy
	if runtime.descriptor.Contract.Defaults != nil {
		documentPolicy = runtime.descriptor.Contract.Defaults.Resilience
	}
	policy := resolvePolicy(documentPolicy, operation.Contract.Resilience, operation.Contract.Idempotency.Kind)
	ctx, cancel := context.WithTimeout(ctx, policy.totalTimeout)
	streamOwnsContext := false
	defer func() {
		if !streamOwnsContext {
			cancel()
		}
	}()

	baseRequest, err := runtime.requestWithBindingHeaders(request, operation)
	if err != nil {
		return nil, err
	}
	baseRequest.OperationID = operation.ID
	baseRequest.MaxResponseBytes = policy.maxResponseBytes
	baseRequest.MaxPayloadBytes = declaredBinaryResponseBytes(operation)
	baseRequest.MaxRequestBytes = declaredBinaryRequestBytes(operation)
	if baseRequest.Headers == nil {
		baseRequest.Headers = make(http.Header)
	}
	baseRequest.Headers.Set("X-Client-Id", runtime.binding.ClientID)
	if requestID := phttp.RequestIDFromContext(ctx); requestID != "" && baseRequest.Headers.Get("X-Request-ID") == "" {
		baseRequest.Headers.Set("X-Request-ID", requestID)
	}
	if keyHeader := operation.Contract.Idempotency.KeyHeader; keyHeader != "" && baseRequest.Headers.Get(keyHeader) == "" {
		key, err := newIdempotencyKey()
		if err != nil {
			return nil, errors.New(CodeClientRequest, "create idempotency key")
		}
		baseRequest.Headers.Set(keyHeader, key)
	}

	breaker := runtime.breaker(operation.ID, policy)
	if err := breaker.AllowRequest(); err != nil {
		return nil, err
	}

	var lastResponse *Response
	var lastErr error
	var lastSecrets []string
	// maxAttempts is a local copy so a forwarded-user remint can grant exactly
	// one extra attempt without disturbing the provider's declared resilience
	// policy for every other caller.
	maxAttempts := policy.maxAttempts
	if baseRequest.BodyStream != nil {
		maxAttempts = 1
	}
	forwardedUserReminted := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay, wait := nextRetryDelay(lastResponse, attempt-1, policy, remainingBudget(ctx), time.Now())
			if !wait {
				// The wait the provider asked for, or the computed backoff, does
				// not fit in the remaining budget. Returning now surfaces the typed
				// remote error instead of replacing it with client.deadline.
				break
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				breaker.onIgnored()
				return nil, normalizedCallError(ctx, ctx.Err())
			}
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, policy.attemptTimeout)
		attempts++
		attemptInfo := ServiceAttemptInfo{
			ServiceCallInfo: ServiceCallInfo{
				ServiceID:   runtime.descriptor.Contract.Service.ID,
				OperationID: operation.ID,
				Protocol:    protocol,
			},
			Attempt: attempt,
		}
		var finishAttempt func(ServiceAttemptResult)
		if telemetry != nil {
			attemptCtx, finishAttempt = telemetry.StartServiceAttempt(attemptCtx, attemptInfo)
		}
		attemptRequest := cloneRequest(baseRequest)
		authStarted := time.Now()
		applied, authErr := runtime.applyCredentials(attemptCtx, attemptRequest, operation.Contract.Security)
		authDuration := time.Since(authStarted)
		if authErr != nil {
			if finishAttempt != nil {
				finishAttempt(ServiceAttemptResult{Code: serviceErrorCode(authErr), AuthDuration: authDuration})
			}
			attemptCancel()
			breaker.onIgnored()
			return nil, authErr
		}
		lastSecrets = applied.secrets
		if telemetry != nil {
			telemetry.InjectServiceContext(attemptCtx, attemptRequest.Headers)
		}
		lastResponse, lastErr = client.transport.Do(attemptCtx, attemptRequest)
		if lastErr == nil && lastResponse != nil && lastResponse.IsSuccess() {
			lastErr = validateSuccessfulResponse(lastResponse, operation.Successes, runtime.descriptor.Schemas)
		}
		if lastErr == nil && lastResponse != nil && lastResponse.IsSuccess() && lastResponse.BodyStream != nil {
			body := newOwnedResponseStream(lastResponse.BodyStream, func() {
				attemptCancel()
				cancel()
			})
			body.finish = func(streamErr error) {
				if finishAttempt == nil {
					return
				}
				if streamErr == nil {
					streamErr = attemptCtx.Err()
				}
				if streamErr != nil && !stderrors.Is(streamErr, io.EOF) && attemptCtx.Err() == nil {
					streamErr = errors.New(CodeClientResponse, "service response stream failed")
				}
				status, code := serviceAttemptOutcome(attemptCtx, lastResponse, streamErr, operation.Contract.Errors)
				finishAttempt(ServiceAttemptResult{StatusCode: status, Code: code, AuthDuration: authDuration})
			}
			if runtime.registry != nil {
				body.registry = runtime.registry
				if err := runtime.registry.trackBinaryStream(body); err != nil {
					if finishAttempt != nil {
						status, code := serviceAttemptOutcome(attemptCtx, lastResponse, err, operation.Contract.Errors)
						finishAttempt(ServiceAttemptResult{StatusCode: status, Code: code, AuthDuration: authDuration})
					}
					body.finish = nil
					body.Close() //nolint:errcheck // The registry refusal remains the primary call error.
					breaker.onIgnored()
					return nil, err
				}
			}
			context.AfterFunc(attemptCtx, func() {
				body.Close() //nolint:errcheck // Cancellation owns cleanup; Close retains its error for the caller.
			})
			lastResponse.BodyStream = body
			streamOwnsContext = true
			breaker.OnSuccess()
			return lastResponse, nil
		}
		if finishAttempt != nil {
			status, code := serviceAttemptOutcome(attemptCtx, lastResponse, lastErr, operation.Contract.Errors)
			finishAttempt(ServiceAttemptResult{StatusCode: status, Code: code, AuthDuration: authDuration})
		}
		closeResponseStream(lastResponse)
		attemptCancel()
		if authenticationRejected(lastResponse, operation.Contract.Errors) {
			runtime.invalidateServiceCredentials(applied.serviceCredentials)
		}
		// A 401 against a forwarded-user credential gets exactly one remint
		// retry when the binding declares Refresh, whether or not the operation
		// declares 401 as a typed Unauthorized error — mirroring a hand-written
		// client's own single-remint contract (e.g. a CLI re-authenticating a
		// stale workspace session) rather than the provider's declared retry
		// policy, which 401 does not participate in by default.
		if baseRequest.BodyStream == nil && !forwardedUserReminted && ctx.Err() == nil && applied.forwardedUserRefresh != nil &&
			lastResponse != nil && lastResponse.StatusCode == http.StatusUnauthorized {
			forwardedUserReminted = true
			if fresh, refreshErr := applied.forwardedUserRefresh(ctx); refreshErr == nil {
				if token := strings.TrimSpace(fresh); token != "" {
					ctx = WithForwardedUserToken(ctx, token)
					maxAttempts++
					continue
				}
			}
		}
		if ctx.Err() != nil || !retryOperation(lastResponse, lastErr, policy, operation.Contract.Errors) || attempt == maxAttempts {
			break
		}
	}

	if lastErr != nil {
		if ctx.Err() != nil {
			breaker.onIgnored()
		} else {
			breaker.OnFailure()
		}
		return nil, normalizedCallError(ctx, lastErr)
	}
	if lastResponse == nil {
		breaker.OnFailure()
		return nil, errors.New(CodeClientResponse, "service returned no response")
	}
	if !lastResponse.IsSuccess() {
		if breaker.isFailureStatus(lastResponse.StatusCode) {
			breaker.OnFailure()
		} else {
			breaker.OnSuccess()
		}
		return nil, decodeRemoteError(runtime.identity(operation), lastResponse, operation.Contract.Errors, runtime.descriptor.Schemas, lastSecrets, runtime.binding.CarryRemoteMessage)
	}
	breaker.OnSuccess()
	return lastResponse, nil
}

// identity names the bound service and the operation for error attribution.
func (runtime *serviceRuntime) identity(operation Operation) callIdentity {
	return callIdentity{serviceID: runtime.descriptor.Contract.Service.ID, operationID: operation.ID}
}

// descriptorIdentity names the call on the descriptor-only path, where the
// service identity comes from the attached generated contract rather than a
// registered binding.
func descriptorIdentity(client *Client, operation Operation) callIdentity {
	identity := callIdentity{operationID: operation.ID}
	if client != nil && client.descriptor != nil {
		identity.serviceID = client.descriptor.Contract.Service.ID
	}
	return identity
}

// nextRetryDelay computes the wait before the next attempt. A provider that
// answers 429 or 503 with Retry-After replaces the exponential backoff with the
// delay it asked for. Either way the wait must fit in the budget left on ctx:
// when it does not, the caller stops retrying instead of sleeping the rest of
// the deadline away.
func nextRetryDelay(response *Response, attempt int, policy effectivePolicy, remaining time.Duration, now time.Time) (time.Duration, bool) {
	if remaining <= 0 {
		return 0, false
	}
	delay := calculateBackoff(attempt, policy.backoffBase, policy.backoffMax)
	if requested, ok := retryAfterDelay(response, now); ok {
		delay = requested
	}
	if delay >= remaining {
		return 0, false
	}
	return delay, true
}

// retryAfterDelay reads the RFC 9110 Retry-After advisory a provider attaches to
// 429 and 503. Both forms are accepted: delta-seconds, and an HTTP-date resolved
// against now. A malformed or past value yields no advisory, so the caller falls
// back to its own backoff instead of retrying immediately.
func retryAfterDelay(response *Response, now time.Time) (time.Duration, bool) {
	if response == nil {
		return 0, false
	}
	if response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusServiceUnavailable {
		return 0, false
	}
	raw := strings.TrimSpace(response.Headers.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	delay := at.Sub(now)
	if delay < 0 {
		return 0, true
	}
	return delay, true
}

// remainingBudget reports the time left on the call deadline. A generated call
// always carries one, so an absent deadline means no wait is affordable.
func remainingBudget(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(deadline)
}

func operationSchemas(client *Client) map[string]clientcontract.Schema {
	if client == nil {
		return nil
	}
	if client.service != nil {
		return client.service.descriptor.Schemas
	}
	if client.descriptor != nil {
		return client.descriptor.Schemas
	}
	return nil
}

func validateGeneratedRequest(request *Request, declared *OperationRequest, schemas map[string]clientcontract.Schema) error {
	if request != nil && request.BodyStream != nil && (declared == nil || binaryRequestContent(declared) == nil) {
		return errors.New(CodeClientRequest, "service request does not declare a binary stream")
	}
	if declared == nil {
		return nil
	}
	if binary := binaryRequestContent(declared); binary != nil {
		// Zero octets are octets: a declared binary body that happens to be
		// empty is a payload, not a missing one, so the emptiness rule below
		// never applies to it.
		return validateBinaryRequest(request, declared, binary)
	}
	if request == nil || len(request.Body) == 0 {
		if declared.Required {
			return errors.New(CodeClientRequest, "service request is missing its required body")
		}
		return nil
	}
	if len(declared.Content) != 1 || declared.Content[0].Schema == nil || !strings.EqualFold(declared.Content[0].MediaType, "application/json") {
		return errors.New(CodeClientConfig, "generated request body contract is invalid")
	}
	mediaType := ""
	if request.Headers != nil {
		mediaType = strings.TrimSpace(strings.Split(request.Headers.Get("Content-Type"), ";")[0])
	}
	if !strings.EqualFold(mediaType, declared.Content[0].MediaType) {
		return errors.New(CodeClientRequest, "service request content type does not match the generated contract")
	}
	if _, ok := projectJSON(request.Body, declared.Content[0].Schema, schemas, nil, false); !ok {
		return errors.New(CodeClientRequest, "service request body does not match the generated contract")
	}
	return nil
}

func validateSuccessfulResponse(response *Response, successes []OperationSuccess, schemas map[string]clientcontract.Schema) error {
	content, err := successContent(response, successes)
	if err != nil {
		return err
	}
	if response.BodyStream != nil && (content == nil || !content.Streamed) {
		return errors.New(CodeClientResponse, "service response unexpectedly contains a binary stream")
	}
	if content != nil && content.IsBinary() {
		if content.Streamed {
			if response.BodyStream == nil || response.Body != nil {
				return errors.New(CodeClientResponse, "service response did not provide the declared binary stream")
			}
			return nil
		}
		// The declared media type matched — that is what successContent proves.
		// There is no document to project: the bound was applied by the
		// transport read, and an empty payload is a legitimate one.
		if content.MaxBytes > 0 && int64(len(response.Body)) > content.MaxBytes {
			return errors.New(CodeClientResponse, "service response exceeds the declared bound")
		}
		return nil
	}
	if len(response.Body) == 0 {
		if content != nil && content.Schema != nil {
			return errors.New(CodeClientResponse, "service returned an empty required response body")
		}
		return nil
	}
	if content == nil || content.Schema == nil {
		return errors.New(CodeClientResponse, "service returned an unexpected response body")
	}
	if _, ok := projectResponseJSON(response.Body, content.Schema, schemas, nil, false); !ok {
		return errors.New(CodeClientResponse, "service response does not match the generated contract")
	}
	return nil
}

func serviceCallResult(response *Response, err error, attempts int) ServiceCallResult {
	result := ServiceCallResult{Attempts: attempts}
	if response != nil {
		result.StatusCode = response.StatusCode
	}
	var remote *RemoteError
	if stderrors.As(err, &remote) {
		result.StatusCode = remote.StatusCode
		result.Code = remote.RemoteCode
		return result
	}
	result.Code = serviceErrorCode(err)
	return result
}

func serviceAttemptOutcome(ctx context.Context, response *Response, err error, declared []clientcontract.DeclaredError) (int, string) {
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return status, serviceErrorCode(normalizedCallError(ctx, err))
	}
	if response == nil {
		return 0, string(CodeClientResponse)
	}
	if response.IsSuccess() {
		return response.StatusCode, ""
	}
	code := remoteCode(response.Body)
	if selected := selectDeclaredError(response.StatusCode, code, declared); selected != nil {
		return response.StatusCode, selected.Code
	}
	return response.StatusCode, string(CodeClientRemote)
}

func serviceErrorCode(err error) string {
	if err == nil {
		return ""
	}
	return string(errors.GetCode(err))
}

func (runtime *serviceRuntime) breaker(operationID string, policy effectivePolicy) *CircuitBreaker {
	key := operationID
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if existing := runtime.breakers[key]; existing != nil {
		return existing
	}
	breaker := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: policy.breakerThreshold,
		ResetTimeout:     policy.breakerReset,
		SuccessThreshold: 1,
		FailureStatuses:  []int{500, 502, 503, 504},
	})
	runtime.breakers[key] = breaker
	return breaker
}

type appliedServiceCredential struct {
	request CredentialRequest
	binding CredentialBinding
	value   string
}

type appliedCredentials struct {
	secrets            []string
	serviceCredentials []appliedServiceCredential
	// forwardedUserRefresh is the binding's remint callback for the
	// CredentialSourceForwardedUser credential applied this attempt, or nil
	// when none was applied or the binding declares no Refresh.
	forwardedUserRefresh func(context.Context) (string, error)
}

// requireDeclaredAnonymous accepts an operation only when the provider declared
// that it can be called with no credential at all, which the contract expresses
// as an alternative with an empty AllOf. An operation with no alternative at all,
// or with only credential-bearing ones, is refused rather than sent anonymously.
func requireDeclaredAnonymous(security clientcontract.Security) error {
	for i := range security.Alternatives {
		if len(security.Alternatives[i].AllOf) == 0 {
			return nil
		}
	}
	return errors.New(CodeClientCredential, "operation declares credentials and requires a registered service binding")
}

func (runtime *serviceRuntime) applyCredentials(ctx context.Context, request *Request, security clientcontract.Security) (appliedCredentials, error) {
	var selected *clientcontract.SecurityAlternative
	for i := range security.Alternatives {
		alternative := &security.Alternatives[i]
		if runtime.alternativeAvailable(ctx, alternative) {
			selected = alternative
			break
		}
	}
	if selected == nil {
		return appliedCredentials{}, errors.New(CodeClientCredential, "no configured credential alternative satisfies the service contract")
	}
	var applied appliedCredentials
	for _, requirement := range selected.AllOf {
		profile := runtime.descriptor.Contract.Credentials[requirement.Profile]
		binding := runtime.binding.Credentials[requirement.Profile]
		switch profile.Kind {
		case clientcontract.CredentialServiceToken:
			audience := runtime.serviceTokenAudience(profile, binding)
			scopes := sortedUnique(append(append([]string(nil), profile.Scopes...), requirement.Scopes...))
			credentialRequest := CredentialRequest{
				ServiceID: runtime.descriptor.Contract.Service.ID,
				ClientID:  runtime.binding.ClientID,
				Profile:   requirement.Profile,
				Audience:  audience,
				Scopes:    scopes,
			}
			credential, err := runtime.credentials.acquire(ctx, credentialRequest, binding, remainingDuration(ctx))
			if err != nil {
				// A stopped registry, a deadline and a cancellation are stable
				// typed outcomes the caller can act on; only an acquisition
				// fault is generalized so a token endpoint cannot describe
				// itself into a caller's error.
				if errors.Is(err, CodeClientClosed) || errors.Is(err, CodeClientDeadline) || errors.Is(err, CodeClientCanceled) {
					return appliedCredentials{}, err
				}
				if ctx.Err() != nil {
					return appliedCredentials{}, normalizedCallError(ctx, err)
				}
				return appliedCredentials{}, errors.New(CodeClientCredential, "service credential acquisition failed")
			}
			if request.Headers.Get("Authorization") != "" {
				return appliedCredentials{}, errors.New(CodeClientCredential, "credential alternative declares conflicting authorization headers")
			}
			request.Headers.Set("Authorization", "Bearer "+credential.Value)
			applied.secrets = append(applied.secrets, credential.Value)
			applied.serviceCredentials = append(applied.serviceCredentials, appliedServiceCredential{
				request: credentialRequest,
				binding: binding,
				value:   credential.Value,
			})
		case clientcontract.CredentialForwardedUserToken:
			if binding.Source != CredentialSourceForwardedUser {
				return appliedCredentials{}, errors.New(CodeClientCredential, "forwarded user credential is not enabled")
			}
			token, ok := forwardedUserToken(ctx)
			if !ok {
				return appliedCredentials{}, errors.New(CodeClientCredential, "forwarded user credential is absent")
			}
			if request.Headers.Get("Authorization") != "" {
				return appliedCredentials{}, errors.New(CodeClientCredential, "credential alternative declares conflicting authorization headers")
			}
			request.Headers.Set("Authorization", "Bearer "+token)
			applied.secrets = append(applied.secrets, token)
			applied.forwardedUserRefresh = binding.Refresh
		case clientcontract.CredentialAPIKey, clientcontract.CredentialNamedHeader:
			if binding.Source != CredentialSourceStatic || binding.Value == "" {
				return appliedCredentials{}, errors.New(CodeClientCredential, "secondary credential is not configured")
			}
			if existing := request.Headers.Get(profile.Header); existing != "" && existing != binding.Value {
				return appliedCredentials{}, errors.New(CodeClientCredential, "credential alternative declares conflicting header values")
			}
			request.Headers.Set(profile.Header, binding.Value)
			applied.secrets = append(applied.secrets, binding.Value)
		}
	}
	return applied, nil
}

// serviceTokenAudience resolves the audience one service-token acquisition
// asks for. The binding wins. gcp-id-token defaults to the binding URL and
// never takes the contract's audience. Every other source keeps the provider's profile, then contract,
// audience. The TypeScript runtime applies the same rule (serviceTokenAudience
// in credential.ts).
func (runtime *serviceRuntime) serviceTokenAudience(profile clientcontract.CredentialProfile, binding CredentialBinding) string {
	if audience := strings.TrimSpace(binding.Audience); audience != "" {
		return audience
	}
	if binding.Source == CredentialSourceGCPIDToken {
		return runtime.baseURL
	}
	return cmp.Or(profile.Audience, runtime.descriptor.Contract.Service.Audience)
}

func (runtime *serviceRuntime) alternativeAvailable(ctx context.Context, alternative *clientcontract.SecurityAlternative) bool {
	for _, requirement := range alternative.AllOf {
		profile, ok := runtime.descriptor.Contract.Credentials[requirement.Profile]
		if !ok {
			return false
		}
		binding, bound := runtime.binding.Credentials[requirement.Profile]
		switch profile.Kind {
		case clientcontract.CredentialForwardedUserToken:
			if !bound || binding.Source != CredentialSourceForwardedUser {
				return false
			}
			if _, ok := forwardedUserToken(ctx); !ok {
				return false
			}
		case clientcontract.CredentialServiceToken:
			if !bound || (binding.Provider == nil && binding.Source != CredentialSourceGCPIDToken &&
				binding.Source != CredentialSourceOAuthClientCredentials && binding.Source != CredentialSourceOAuthClientAssertion &&
				binding.Source != CredentialSourceOAuthExtensionGrant) {
				return false
			}
		case clientcontract.CredentialAPIKey, clientcontract.CredentialNamedHeader:
			if !bound || binding.Source != CredentialSourceStatic || binding.Value == "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (runtime *serviceRuntime) invalidateServiceCredentials(applied []appliedServiceCredential) {
	for _, credential := range applied {
		runtime.credentials.invalidate(credential.request, credential.binding, credential.value)
	}
}

func authenticationRejected(response *Response, declared []clientcontract.DeclaredError) bool {
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		return false
	}
	selected := selectDeclaredError(response.StatusCode, remoteCode(response.Body), declared)
	return selected != nil && selected.Code == string(errors.CodeUnauthorized)
}

func retryOperation(response *Response, err error, policy effectivePolicy, declared []clientcontract.DeclaredError) bool {
	if err != nil {
		// Response decoding/size failures are deterministic for a given response
		// and must not be turned into retry storms. Generated HTTP transports wrap
		// actual dial/write failures as client.request.
		return errors.Is(err, CodeClientRequest)
	}
	if response == nil {
		return true
	}
	code := remoteCode(response.Body)
	if selected := selectDeclaredError(response.StatusCode, code, declared); selected != nil && selected.Retryable != nil {
		return *selected.Retryable
	}
	return slices.Contains(policy.retryStatuses, response.StatusCode) ||
		(code != "" && slices.Contains(policy.retryCodes, code))
}

func cloneRequest(request *Request) *Request {
	if request == nil {
		return &Request{}
	}
	copy := *request
	copy.Headers = request.Headers.Clone()
	copy.Body = append([]byte(nil), request.Body...)
	if request.Query != nil {
		copy.Query = make(map[string]string, len(request.Query))
		for key, value := range request.Query {
			copy.Query[key] = value
		}
	}
	copy.QueryValues = cloneURLValues(request.QueryValues)
	return &copy
}

func remainingDuration(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return defaultCredentialTimeout
}

func newIdempotencyKey() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

const (
	// CodeClientCanceled identifies a caller-canceled generated service call.
	CodeClientCanceled errors.Code = "client.canceled"
	// CodeClientDeadline identifies a generated service call whose total or attempt budget expired.
	CodeClientDeadline errors.Code = "client.deadline"
)

func normalizedCallError(ctx context.Context, source error) error {
	if ctx.Err() == context.Canceled {
		return errors.New(CodeClientCanceled, "service call was canceled")
	}
	if ctx.Err() == context.DeadlineExceeded {
		return errors.New(CodeClientDeadline, "service call deadline exceeded")
	}
	if stderrors.Is(source, context.Canceled) {
		return errors.New(CodeClientCanceled, "service call was canceled")
	}
	if stderrors.Is(source, context.DeadlineExceeded) {
		return errors.New(CodeClientDeadline, "service call deadline exceeded")
	}
	if errors.Is(source, CodeClientResponse) {
		return errors.New(CodeClientResponse, "service transport response failed")
	}
	return errors.New(CodeClientRequest, "service transport request failed")
}

type forwardedUserTokenKey struct{}

// WithForwardedUserToken explicitly carries an inbound user bearer token to a
// generated call. The token is context-scoped and is never stored in a client
// or the service credential cache.
func WithForwardedUserToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, forwardedUserTokenKey{}, strings.TrimSpace(token))
}

func forwardedUserToken(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(forwardedUserTokenKey{}).(string)
	if ok && token != "" {
		return token, true
	}
	return phttp.ForwardedBearerTokenFromContext(ctx)
}

// Call decodes a successful generated unary response into T.
func Call[T any](ctx context.Context, client *Client, request *Request, operation Operation) (T, error) {
	var output T
	response, err := client.DoOperation(ctx, request, operation)
	if err != nil {
		return output, err
	}
	return decodeCallResponse[T](client, response, operation)
}

// decodeCallResponse decodes the successful response of one generated unary
// call into T. A cached response goes through it exactly like a fresh one.
func decodeCallResponse[T any](client *Client, response *Response, operation Operation) (T, error) {
	var output T
	content, err := successContent(response, operation.Successes)
	if err != nil {
		return output, err
	}
	if len(response.Body) == 0 {
		if content != nil && content.Schema != nil {
			return output, errors.New(CodeClientResponse, "service returned an empty required response body")
		}
		return output, nil
	}
	if content == nil {
		return output, errors.New(CodeClientResponse, "service returned an unexpected response body")
	}
	return DecodeResponse[T](client, response, operation)
}

// DecodeResponse validates and decodes a generated operation response.
func DecodeResponse[T any](client *Client, response *Response, operation Operation) (T, error) {
	var output T
	content, err := successContent(response, operation.Successes)
	if err != nil {
		return output, err
	}
	if content == nil || content.Schema == nil {
		return output, errors.New(CodeClientResponse, "service response has no generated schema")
	}
	if content.IsBinary() {
		// Raw octets have no JSON projection. Decoding them here would mean
		// guessing an encoding the provider never declared; CallOperationBinary
		// is the path that hands them over unchanged.
		return output, errors.New(CodeClientConfig, "raw octet response requires the binary call path")
	}
	var schemas map[string]clientcontract.Schema
	if client != nil && client.service != nil {
		schemas = client.service.descriptor.Schemas
	} else if client != nil && client.descriptor != nil {
		schemas = client.descriptor.Schemas
	}
	if _, ok := projectResponseJSON(response.Body, content.Schema, schemas, nil, false); !ok {
		return output, errors.New(CodeClientResponse, "service response does not match the generated contract")
	}
	// The typed value is decoded from the provider's bytes, so an opaque member
	// keeps them unchanged. A generated type declares a closed object as a
	// struct, which binds only the properties the schema declares.
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil {
		return output, errors.New(CodeClientResponse, "service returned an invalid response body")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return output, errors.New(CodeClientResponse, "service returned trailing response data")
	}
	return output, nil
}

// checkVoidResponse refuses a body on a generated operation that declares none.
func checkVoidResponse(response *Response, operation Operation) error {
	content, err := successContent(response, operation.Successes)
	if err != nil {
		return err
	}
	if content != nil || len(response.Body) != 0 {
		return errors.New(CodeClientResponse, "service returned an unexpected response body")
	}
	return nil
}

// EncodeJSON encodes a generated request body while keeping arbitrary
// MarshalJSON errors and values out of the public error chain.
func EncodeJSON(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New(CodeClientRequest, "service request body does not match the generated contract")
	}
	return body, nil
}
