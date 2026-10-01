package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// ServiceCacheTelemetry is the optional extension of ServiceCallTelemetry a
// telemetry bridge implements to observe the response cache. It is separate so
// an existing bridge keeps compiling; one that does not implement it simply
// records no cache metric.
type ServiceCacheTelemetry interface {
	// ServiceCacheStaleServed records one stored answer returned after the
	// provider call failed, and how old that answer was.
	ServiceCacheStaleServed(ctx context.Context, info ServiceCallInfo, age time.Duration)
}

// runtimeCapabilities is the closed set of contract capabilities this runtime
// implements. It equals the Go row of the contract
// (clientcontract.RuntimeCapabilitiesImplementedBy); a test pins the two
// together.
var runtimeCapabilities = []clientcontract.RuntimeCapability{
	clientcontract.RuntimeCapabilityResponseCache,
	clientcontract.RuntimeCapabilitySSEContinuation,
}

// RuntimeCapabilities reports the contract capabilities this runtime
// implements, in ascending order.
func RuntimeCapabilities() []clientcontract.RuntimeCapability {
	return append([]clientcontract.RuntimeCapability(nil), runtimeCapabilities...)
}

// RequireRuntimeCapabilities is called by generated code that needs a
// capability beyond the base runtime. A runtime older than the capability has
// no such function, so the generated package fails to compile against it; a
// runtime that has the function but not the named capability panics when the
// generated package initializes. Either way the target never runs without the
// behavior its contract declares.
func RequireRuntimeCapabilities(required ...string) bool {
	for _, capability := range required {
		if !slices.Contains(runtimeCapabilities, clientcontract.RuntimeCapability(capability)) {
			panic(fmt.Sprintf("generated client requires runtime capability %q, which go.putnami.dev/client does not implement", capability))
		}
	}
	return true
}

// responseCaches is the response cache an application's service registry
// owns. It holds one bounded least-recently-used cache per service,
// operation and declared policy, shared by every client bound from the
// registry, and it ends with the registry.
type responseCaches struct {
	mu     sync.Mutex
	closed bool
	caches map[string]*operationCache

	// now is the cache clock. Tests move it instead of sleeping for a real
	// fresh or stale window.
	now func() time.Time
}

func newResponseCaches() *responseCaches {
	return newResponseCachesWithClock(time.Now)
}

func newResponseCachesWithClock(now func() time.Time) *responseCaches {
	return &responseCaches{caches: make(map[string]*operationCache), now: now}
}

type cachePolicy struct {
	fresh      time.Duration
	stale      time.Duration
	maxEntries int
	// invalidationFields are the response properties that tag each stored
	// answer, so an invalidation by one of them finds it (ADR 0007 of the client
	// contract).
	invalidationFields []string
}

func resolveCachePolicy(declared *clientcontract.CachePolicy) cachePolicy {
	policy := cachePolicy{
		fresh:      time.Duration(declared.FreshMs) * time.Millisecond,
		maxEntries: clientcontract.DefaultCacheMaxEntries,
	}
	if declared.StaleMs != nil {
		policy.stale = time.Duration(*declared.StaleMs) * time.Millisecond
	}
	if declared.MaxEntries != nil {
		policy.maxEntries = *declared.MaxEntries
	}
	policy.invalidationFields = slices.Clone(declared.InvalidationFields)
	return policy
}

// retention is the age past which an entry can answer nothing.
func (policy cachePolicy) retention() time.Duration {
	return max(policy.fresh, policy.stale)
}

// operationCache is one operation's entries on one service binding. An entry
// slot is the forwarded identity and the canonical key; the key alone is what
// an invalidation prefix matches. The entries form a recency list from newest
// to oldest, so the bound evicts the least recently used one.
type operationCache struct {
	serviceID string
	policy    cachePolicy
	entries   map[string]*cacheEntry
	newest    *cacheEntry
	oldest    *cacheEntry
	flights   map[string]*responseFlight
}

type cacheEntry struct {
	slot     string
	key      string
	response *Response
	// tags maps each declared invalidation field the stored answer carries to
	// its canonical value; a field the answer does not carry is absent.
	tags     map[string]string
	storedAt time.Time
	newer    *cacheEntry
	older    *cacheEntry
}

// responseFlight is the one upstream call a key and identity has in flight.
// Every caller that misses meanwhile waits for it instead of calling again.
type responseFlight struct {
	key    string
	done   chan struct{}
	cancel context.CancelFunc
	// detached marks a flight an invalidation overtook: its callers still get
	// its answer, but the answer is never stored and nobody else joins it.
	detached bool
	response *Response
	err      error
}

func (caches *responseCaches) operation(serviceID, operationID string, policy cachePolicy) *operationCache {
	// Field names hold no control character, so NUL separates them unambiguously.
	name := serviceID + "\x00" + operationID + "\x00" + strconv.FormatInt(int64(policy.fresh), 10) + "/" +
		strconv.FormatInt(int64(policy.stale), 10) + "/" + strconv.Itoa(policy.maxEntries) + "\x00" +
		strings.Join(policy.invalidationFields, "\x00")
	cache := caches.caches[name]
	if cache == nil {
		cache = &operationCache{
			serviceID: serviceID,
			policy:    policy,
			entries:   make(map[string]*cacheEntry),
			flights:   make(map[string]*responseFlight),
		}
		caches.caches[name] = cache
	}
	return cache
}

// serve answers one call from the cache or through the single upstream call
// in flight for its slot. call is the rest of the chain; stale decides
// whether a failure may be masked by a stored answer.
func (caches *responseCaches) serve(
	ctx context.Context,
	serviceID, operationID string,
	policy cachePolicy,
	identity, key string,
	call func(context.Context) (*Response, error),
	stale func(context.Context, error) bool,
	servedStale func(context.Context, time.Duration, error),
) (*Response, error) {
	// A caller that already walked away or ran out of time starts nothing and
	// is handed nothing, stored or not.
	if err := ctx.Err(); err != nil {
		return nil, normalizedCallError(ctx, err)
	}
	slot := identity + "\x00" + key
	caches.mu.Lock()
	if caches.closed {
		caches.mu.Unlock()
		return nil, errClosedRegistry()
	}
	cache := caches.operation(serviceID, operationID, policy)
	now := caches.now()
	if entry := cache.lookup(slot, now); entry != nil && now.Sub(entry.storedAt) < policy.fresh {
		cache.touch(entry)
		response := cloneResponse(entry.response)
		caches.mu.Unlock()
		return response, nil
	}
	flight := cache.flights[slot]
	if flight == nil {
		// The call is detached from the first caller's cancellation and
		// deadline, so one caller walking away or running out of time cannot
		// fail every other caller waiting on it. It keeps the caller's values —
		// the forwarded identity the slot is keyed on, the trace — and stays
		// bounded by the operation's own declared budget. Closing the registry
		// cancels it.
		flightCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &responseFlight{key: key, done: make(chan struct{}), cancel: cancel}
		cache.flights[slot] = flight
		go caches.fly(cache, slot, flight, flightCtx, call)
	}
	caches.mu.Unlock()

	select {
	case <-flight.done:
	case <-ctx.Done():
	}
	if err := ctx.Err(); err != nil {
		// An explicit cancellation is always the caller's answer. A caller whose
		// own deadline passed while the provider hung is the outage the stale
		// window exists for: it takes the stored answer, and the shared call
		// keeps running for the callers still waiting on it.
		if !stderrors.Is(err, context.DeadlineExceeded) {
			return nil, normalizedCallError(ctx, err)
		}
		select {
		case <-flight.done:
			if flight.err == nil {
				return cloneResponse(flight.response), nil
			}
		default:
		}
		deadline := normalizedCallError(ctx, err)
		if response, age, ok := caches.staleAnswer(cache, slot, policy); ok {
			servedStale(ctx, age, deadline)
			return response, nil
		}
		return nil, deadline
	}
	if flight.err == nil {
		return cloneResponse(flight.response), nil
	}
	if !stale(ctx, flight.err) {
		return nil, flight.err
	}
	if response, age, ok := caches.staleAnswer(cache, slot, policy); ok {
		servedStale(ctx, age, flight.err)
		return response, nil
	}
	return nil, flight.err
}

// staleAnswer reads the slot's entry again, when the policy declares a stale
// window. An invalidation that landed while the call was in flight removed
// the entry, and a removed answer is never served, stale or not.
func (caches *responseCaches) staleAnswer(cache *operationCache, slot string, policy cachePolicy) (*Response, time.Duration, bool) {
	if policy.stale <= 0 {
		return nil, 0, false
	}
	caches.mu.Lock()
	defer caches.mu.Unlock()
	now := caches.now()
	entry := cache.lookup(slot, now)
	if caches.closed || entry == nil || now.Sub(entry.storedAt) >= policy.stale {
		return nil, 0, false
	}
	return cloneResponse(entry.response), now.Sub(entry.storedAt), true
}

func (caches *responseCaches) fly(cache *operationCache, slot string, flight *responseFlight, ctx context.Context, call func(context.Context) (*Response, error)) {
	defer flight.cancel()
	response, err := call(ctx)
	// The tags are read before the lock: decoding a body is the one piece of
	// work here that grows with the answer, and the policy never changes.
	var tags map[string]string
	if err == nil {
		tags = responseTags(response, cache.policy.invalidationFields)
	}
	caches.mu.Lock()
	defer caches.mu.Unlock()
	if cache.flights[slot] == flight {
		delete(cache.flights, slot)
	}
	switch {
	case caches.closed:
		// The registry closed while the call was running. Its answer does not
		// outlive the application that asked for it.
		response, err = nil, errClosedRegistry()
	case err == nil && !flight.detached:
		cache.store(slot, flight.key, tags, cloneResponse(response), caches.now())
	}
	flight.response, flight.err = response, err
	close(flight.done)
}

// lookup returns the slot's entry, dropping it first when it is too old to
// answer anything.
func (cache *operationCache) lookup(slot string, now time.Time) *cacheEntry {
	entry := cache.entries[slot]
	if entry == nil {
		return nil
	}
	if now.Sub(entry.storedAt) >= cache.policy.retention() {
		cache.remove(entry)
		return nil
	}
	return entry
}

func (cache *operationCache) store(slot, key string, tags map[string]string, response *Response, now time.Time) {
	if entry := cache.entries[slot]; entry != nil {
		entry.response, entry.tags, entry.storedAt = response, tags, now
		cache.touch(entry)
		return
	}
	entry := &cacheEntry{slot: slot, key: key, response: response, tags: tags, storedAt: now}
	cache.entries[slot] = entry
	cache.pushNewest(entry)
	for len(cache.entries) > cache.policy.maxEntries {
		cache.remove(cache.oldest)
	}
}

// touch makes entry the most recently used one.
func (cache *operationCache) touch(entry *cacheEntry) {
	cache.unlink(entry)
	cache.pushNewest(entry)
}

func (cache *operationCache) remove(entry *cacheEntry) {
	cache.unlink(entry)
	delete(cache.entries, entry.slot)
}

func (cache *operationCache) pushNewest(entry *cacheEntry) {
	entry.newer, entry.older = nil, cache.newest
	if cache.newest != nil {
		cache.newest.newer = entry
	}
	cache.newest = entry
	if cache.oldest == nil {
		cache.oldest = entry
	}
}

func (cache *operationCache) unlink(entry *cacheEntry) {
	if entry.newer != nil {
		entry.newer.older = entry.older
	} else if cache.newest == entry {
		cache.newest = entry.older
	}
	if entry.older != nil {
		entry.older.newer = entry.newer
	} else if cache.oldest == entry {
		cache.oldest = entry.newer
	}
	entry.newer, entry.older = nil, nil
}

// invalidate drops every entry of the service whose key starts with prefix,
// for every identity, and detaches every matching call in flight.
func (caches *responseCaches) invalidate(serviceID, prefix string) int {
	caches.mu.Lock()
	defer caches.mu.Unlock()
	dropped := 0
	for _, cache := range caches.caches {
		if cache.serviceID != serviceID {
			continue
		}
		for _, entry := range cache.entries {
			if strings.HasPrefix(entry.key, prefix) {
				cache.remove(entry)
				dropped++
			}
		}
		for slot, flight := range cache.flights {
			if strings.HasPrefix(flight.key, prefix) {
				flight.detached = true
				delete(cache.flights, slot)
			}
		}
	}
	return dropped
}

// invalidateByField drops every entry of the service whose declared field
// carries tag, for every identity and across its operations. A call in flight
// for an operation that declares the field has no answer yet, so nothing says
// that answer will not carry the value: it is detached, its answer is never
// stored, and the next caller goes upstream.
func (caches *responseCaches) invalidateByField(serviceID, field, tag string) int {
	caches.mu.Lock()
	defer caches.mu.Unlock()
	dropped := 0
	for _, cache := range caches.caches {
		if cache.serviceID != serviceID || !slices.Contains(cache.policy.invalidationFields, field) {
			continue
		}
		for _, entry := range cache.entries {
			if value, ok := entry.tags[field]; ok && value == tag {
				cache.remove(entry)
				dropped++
			}
		}
		for slot, flight := range cache.flights {
			flight.detached = true
			delete(cache.flights, slot)
		}
	}
	return dropped
}

// close drops every entry, cancels every call in flight and refuses every
// later lookup. It is idempotent.
func (caches *responseCaches) close() {
	if caches == nil {
		return
	}
	caches.mu.Lock()
	if caches.closed {
		caches.mu.Unlock()
		return
	}
	caches.closed = true
	var cancels []context.CancelFunc
	for _, cache := range caches.caches {
		for _, flight := range cache.flights {
			cancels = append(cancels, flight.cancel)
		}
	}
	clear(caches.caches)
	caches.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// InvalidateResponses drops every cached answer of the service whose key starts
// with keyPrefix, for every forwarded identity, and returns how many it
// dropped. A call already in flight for a matching key still answers the
// callers waiting on it, but its answer is never stored, so the next call goes
// upstream. An empty prefix drops every answer of the service.
//
// The key is "<operationId>?<field>=<value>&…" as ADR 0007 of the client
// contract defines it, so "effectiveAccess?" drops one operation and
// "effectiveAccess?body.principal=%22p1%22" one principal's answers when
// body.principal is the operation's first key field.
func (bindings *ServiceBindings) InvalidateResponses(serviceID, keyPrefix string) int {
	if bindings == nil || bindings.responses == nil {
		return 0
	}
	return bindings.responses.invalidate(serviceID, keyPrefix)
}

// InvalidateResponses drops the cached answers of this client's service whose
// key starts with keyPrefix. See ServiceBindings.InvalidateResponses.
func (c *Client) InvalidateResponses(keyPrefix string) int {
	if c == nil || c.service == nil || c.service.registry == nil {
		return 0
	}
	return c.service.registry.InvalidateResponses(c.service.descriptor.Contract.Service.ID, keyPrefix)
}

// InvalidateResponsesByField drops every cached answer of the service whose
// declared invalidation field carries value, across the service's operations
// and for every forwarded identity, and returns how many it dropped. A call in
// flight for an operation that declares the field still answers the callers
// waiting on it, but its answer is never stored, so the next call goes
// upstream.
//
// It is how a consumer revokes by a value the request never carried — a
// principal id the answer names while the request named an issuer and a
// subject. The provider lists the field in the operation's
// resilience.cache.invalidationFields (ADR 0007 of the client contract). value
// is a string, a boolean or an integer of any width, a named type of one of
// them, or a json.Number holding an integer; it is compared with the stored
// answer's field in the canonical form both runtimes render, so the string
// "42" and the integer 42 are different values. Any other value is refused
// with client.config, never silently matched against nothing. A field no
// operation of the service declares drops nothing.
func (bindings *ServiceBindings) InvalidateResponsesByField(serviceID, field string, value any) (int, error) {
	tag, err := invalidationTag(field, value)
	if err != nil {
		return 0, err
	}
	if bindings == nil || bindings.responses == nil {
		return 0, nil
	}
	return bindings.responses.invalidateByField(serviceID, field, tag), nil
}

// InvalidateResponsesByField drops the cached answers of this client's service
// whose declared invalidation field carries value. See
// ServiceBindings.InvalidateResponsesByField.
func (c *Client) InvalidateResponsesByField(field string, value any) (int, error) {
	if c == nil || c.service == nil || c.service.registry == nil {
		if _, err := invalidationTag(field, value); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return c.service.registry.InvalidateResponsesByField(c.service.descriptor.Contract.Service.ID, field, value)
}

type withoutResponseCacheKey struct{}

// WithoutResponseCache returns a context whose generated calls neither read
// nor store the declared response cache, nor wait on a call already in
// flight: each goes to the provider and returns what the provider answers.
// A route that must act on the current answer — a mutation checking access
// before it writes — calls through it. A failure is never masked by a stored
// answer, stale or not. The switch travels with the context, like
// WithForwardedUserToken, so every generated call made with it is bypassed.
func WithoutResponseCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutResponseCacheKey{}, true)
}

// responseCacheBypassed reports whether ctx carries WithoutResponseCache.
func responseCacheBypassed(ctx context.Context) bool {
	bypassed, ok := ctx.Value(withoutResponseCacheKey{}).(bool)
	return ok && bypassed
}

// invalidationTag checks an invalidation's field and renders its value in the
// canonical form stored answers are tagged with.
func invalidationTag(field string, value any) (string, error) {
	if err := clientcontract.ParseCacheInvalidationField(field); err != nil {
		return "", errors.New(CodeClientConfig, err.Error())
	}
	tag, ok := cacheFieldValue(value)
	if !ok {
		return "", errors.Newf(CodeClientConfig,
			"invalidation field %q takes a string, an integer or a boolean, not %T", field, value)
	}
	return tag, nil
}

// responseTags reads each declared invalidation field from a stored JSON
// object body. A field the body does not carry, carries as null, or carries as
// anything but a string, an integer or a boolean tags nothing; so does a body
// that is not a JSON object.
func responseTags(response *Response, fields []string) map[string]string {
	if len(fields) == 0 || response == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil
	}
	var tags map[string]string
	for _, field := range fields {
		if tag, ok := cacheFieldValue(object[field]); ok {
			if tags == nil {
				tags = make(map[string]string, len(fields))
			}
			tags[field] = tag
		}
	}
	return tags
}

// cacheFieldValue renders one invalidation value canonically (ADR 0007): a
// string as JSON.stringify quotes it, a boolean as true or false, an integer as
// its decimal digits with no sign on zero. A json.Number counts only when it
// is an integer lexeme. Every other value, null included, has no tag. The
// shared vectors in protocols/clientcontract/fixtures/cache pin the rendering.
func cacheFieldValue(value any) (string, bool) {
	if number, ok := value.(json.Number); ok {
		return canonicalIntegerLexeme(number.String())
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.String:
		return canonicalJSONString(reflected.String()), true
	case reflect.Bool:
		return strconv.FormatBool(reflected.Bool()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(reflected.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(reflected.Uint(), 10), true
	default:
		return "", false
	}
}

// canonicalIntegerLexeme accepts a JSON integer lexeme and renders it: a JSON
// integer already has no leading zero and no exponent, so only negative zero
// changes.
func canonicalIntegerLexeme(lexeme string) (string, bool) {
	digits := strings.TrimPrefix(lexeme, "-")
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return "", false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return "", false
		}
	}
	if digits == "0" {
		return "0", true
	}
	return lexeme, true
}

// cachePlan is what the response cache resolves once per operation: the
// validated contract's resilience and cache policies and the call identity it
// reports under. A nil plan means the contract does not validate and the
// operation is never cached.
type cachePlan struct {
	resilience effectivePolicy
	policy     cachePolicy
	info       ServiceCallInfo
}

// cachePlanKey names one generated operation. A generated Operation is parsed
// once into a package variable and passed by value on every call, so its
// resilience pointer is the same for every call of the same declaration.
type cachePlanKey struct {
	operationID string
	resilience  *clientcontract.ResiliencePolicy
}

// cachePlan resolves the operation's plan on its first call and reuses it on
// every later one, so a cache hit repeats no per-operation work.
func (runtime *serviceRuntime) cachePlan(operation Operation) *cachePlan {
	key := cachePlanKey{operationID: operation.ID, resilience: operation.Contract.Resilience}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if plan, resolved := runtime.cachePlans[key]; resolved {
		return plan
	}
	if runtime.cachePlans == nil {
		runtime.cachePlans = make(map[cachePlanKey]*cachePlan)
	}
	var plan *cachePlan
	// An operation whose contract does not validate is left to DoOperation,
	// which refuses it: an invalid declaration must never be cached around.
	if diagnostics := clientcontract.ValidateOperationForID(operation.ID, &operation.Contract, &runtime.descriptor.Contract); len(diagnostics) == 0 {
		var documentPolicy *clientcontract.ResiliencePolicy
		if runtime.descriptor.Contract.Defaults != nil {
			documentPolicy = runtime.descriptor.Contract.Defaults.Resilience
		}
		protocol := string(clientcontract.TransportRESTJSON)
		if transport, err := dispatchTransport(operation, supportedUnaryTransport); err == nil {
			protocol = string(transport.Protocol)
		}
		plan = &cachePlan{
			resilience: resolvePolicy(documentPolicy, operation.Contract.Resilience, operation.Contract.Idempotency.Kind),
			policy:     resolveCachePolicy(operation.Contract.Resilience.Cache),
			info:       ServiceCallInfo{ServiceID: runtime.descriptor.Contract.Service.ID, OperationID: operation.ID, Protocol: protocol},
		}
	}
	runtime.cachePlans[key] = plan
	return plan
}

// responseCacheInterceptor returns the interceptor that answers one generated
// unary call from its operation's declared response cache, or nil when the
// operation declares none or the client has no registry to own a cache.
func responseCacheInterceptor(client *Client, call *OperationCall, operation Operation) Interceptor {
	if client == nil || client.service == nil || client.service.registry == nil || client.service.registry.responses == nil ||
		call == nil || call.Request == nil || operation.Contract.Resilience == nil || operation.Contract.Resilience.Cache == nil {
		return nil
	}
	runtime := client.service
	plan := runtime.cachePlan(operation)
	if plan == nil {
		return nil
	}
	return func(ctx context.Context, request *Request, next InterceptorFunc) (*Response, error) {
		// A stored answer never stands in for a request the provider's schema
		// refuses: the request the key is read from is validated before the
		// lookup, exactly as before a call.
		if err := validateGeneratedRequest(call.Request, operation.Request, operationSchemas(client)); err != nil {
			return nil, err
		}
		if runtime.registry.isClosed() {
			return nil, errClosedRegistry()
		}
		// The key reads REST fields even when Connect carries the request. Only
		// headers need a private effective-value projection here; body, path and
		// query are read-only. DoOperation separately owns the transport copy,
		// including for a shared flight that outlives this caller's deadline.
		keyCall := *call
		keyRequest := *call.Request
		var err error
		keyRequest.Headers, err = runtime.applyBindingHeaders(call.Request.Headers.Clone(), operation)
		if err != nil {
			return nil, err
		}
		keyCall.Request = &keyRequest
		key, err := responseCacheKey(operation, &keyCall)
		if err != nil {
			return nil, err
		}
		identity, err := json.Marshal([]string{runtime.baseURL, forwardedIdentity(ctx)})
		if err != nil {
			return nil, errors.New(CodeClientConfig, "encode endpoint cache identity")
		}
		return runtime.registry.responses.serve(ctx, plan.info.ServiceID, operation.ID, plan.policy, string(identity), key,
			func(ctx context.Context) (*Response, error) { return next(ctx, request) },
			func(ctx context.Context, err error) bool {
				return staleServable(ctx, err, plan.resilience, operation.Contract.Errors)
			},
			func(ctx context.Context, age time.Duration, cause error) {
				reportStaleServed(ctx, plan.info, age, cause)
			},
		)
	}
}

// staleServable reports whether a failed call is one a stored answer may
// mask: the provider could not be reached or kept failing. A caller that
// canceled, a request the provider refused, and a response that broke the
// contract all reach the caller.
func staleServable(ctx context.Context, err error, policy effectivePolicy, declared []clientcontract.DeclaredError) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, CodeClientRequest) || errors.Is(err, CodeClientDeadline) || errors.Is(err, CodeCircuitOpen) {
		return true
	}
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		return false
	}
	if selected := selectDeclaredError(remote.StatusCode, remote.RemoteCode, declared); selected != nil && selected.Retryable != nil {
		return *selected.Retryable
	}
	return slices.Contains(policy.retryStatuses, remote.StatusCode) ||
		(remote.RemoteCode != "" && slices.Contains(policy.retryCodes, remote.RemoteCode))
}

// reportStaleServed makes a stale answer observable: one metric through the
// installed telemetry bridge, and one warning log line.
func reportStaleServed(ctx context.Context, info ServiceCallInfo, age time.Duration, cause error) {
	if observer, ok := currentServiceTelemetry().(ServiceCacheTelemetry); ok {
		observer.ServiceCacheStaleServed(ctx, info, age)
	}
	logger.FromContextOrDefault(ctx).Named("client").WarnCtx(ctx, "served a stale cached response after a provider failure",
		slog.String("rpc.service", info.ServiceID),
		slog.String("rpc.method", info.OperationID),
		slog.Int64("ageMs", age.Milliseconds()),
		slog.String("error.type", serviceCallResult(nil, cause, 0).Code),
	)
}

// forwardedIdentity is the identity half of a cache slot: the SHA-256 of the
// forwarded user token the call carries, or empty when it carries none. The
// token itself never enters the cache.
func forwardedIdentity(ctx context.Context) string {
	token, ok := forwardedUserToken(ctx)
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func cloneResponse(response *Response) *Response {
	if response == nil {
		return nil
	}
	return &Response{
		StatusCode: response.StatusCode,
		Headers:    response.Headers.Clone(),
		Body:       append([]byte(nil), response.Body...),
	}
}

// responseCacheKey renders the canonical key of one generated call (ADR 0007
// of the client contract): "<operationId>?" followed by the declared key
// fields in order, or by every path, query and header parameter and the whole
// body when none are declared.
func responseCacheKey(operation Operation, call *OperationCall) (string, error) {
	policy := operation.Contract.Resilience.Cache
	request := call.Request
	query := cloneURLValues(request.QueryValues)
	if query == nil {
		query = url.Values{}
	}
	for name, value := range request.Query {
		query.Set(name, value)
	}
	body, bodyErr := canonicalRequestBody(request.Body, binaryRequestContent(operation.Request) != nil)
	if bodyErr != nil {
		return "", errors.New(CodeClientRequest, "service request body cannot form a cache key")
	}
	return canonicalCacheKey(operation.ID, policy.KeyFields, operation.Contract.Idempotency.KeyHeader,
		call.PathParams, query, request.Headers, body), nil
}

// canonicalCacheKey is the runtime-independent half of the key: the same
// inputs render the same string in the Go and the TypeScript runtime, which
// the shared vectors in protocols/clientcontract/fixtures/cache pin.
func canonicalCacheKey(operationID string, keyFields []string, keyHeader string, path map[string]string, query url.Values, headers http.Header, body *string) string {
	var parts []string
	add := func(field string, value *string) {
		if value == nil {
			parts = append(parts, field)
			return
		}
		parts = append(parts, field+"="+cacheKeyEscape(*value))
	}
	lowerHeaders := map[string][]string{}
	for name, values := range headers {
		lower := strings.ToLower(name)
		if lower == "content-type" || (keyHeader != "" && lower == strings.ToLower(keyHeader)) {
			continue
		}
		// A repeated field and its ", "-combined form are the same field (the
		// combination rule HTTP and Fetch apply), so each value is split at that
		// separator: http.Header hands a repeated field over repeated, Headers
		// hands it over combined, and both render one JSON array.
		for _, value := range values {
			lowerHeaders[lower] = append(lowerHeaders[lower], strings.Split(value, ", ")...)
		}
	}
	if len(keyFields) == 0 {
		for _, name := range sortedKeys(path) {
			value := canonicalJSONString(path[name])
			add("path."+name, &value)
		}
		for _, name := range sortedKeys(query) {
			add("query."+name, canonicalStrings(query[name]))
		}
		for _, name := range sortedKeys(lowerHeaders) {
			add("header."+name, canonicalStrings(lowerHeaders[name]))
		}
		if body != nil {
			add("body", body)
		}
		return operationID + "?" + strings.Join(parts, "&")
	}
	for _, field := range keyFields {
		section, name, err := clientcontract.ParseCacheKeyField(field)
		if err != nil {
			add(field, nil)
			continue
		}
		switch section {
		case clientcontract.CacheKeySectionPath:
			if value, ok := path[name]; ok {
				rendered := canonicalJSONString(value)
				add(field, &rendered)
			} else {
				add(field, nil)
			}
		case clientcontract.CacheKeySectionQuery:
			add(field, canonicalStrings(query[name]))
		case clientcontract.CacheKeySectionHeader:
			add(field, canonicalStrings(lowerHeaders[strings.ToLower(name)]))
		case clientcontract.CacheKeySectionBody:
			add(field, bodyField(body, name))
		}
	}
	return operationID + "?" + strings.Join(parts, "&")
}

func sortedKeys[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// canonicalStrings renders one value as a JSON string and several as a JSON
// array of strings; no value is absent.
func canonicalStrings(values []string) *string {
	switch len(values) {
	case 0:
		return nil
	case 1:
		rendered := canonicalJSONString(values[0])
		return &rendered
	}
	rendered := make([]string, len(values))
	for i, value := range values {
		rendered[i] = canonicalJSONString(value)
	}
	joined := "[" + strings.Join(rendered, ",") + "]"
	return &joined
}

// bodyField renders one top-level property of a canonical JSON object body.
func bodyField(body *string, name string) *string {
	if body == nil {
		return nil
	}
	if name == "" {
		return body
	}
	decoder := json.NewDecoder(strings.NewReader(*body))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil
	}
	value, ok := object[name]
	if !ok {
		return nil
	}
	var rendered bytes.Buffer
	writeCanonicalJSON(&rendered, value)
	text := rendered.String()
	return &text
}

// canonicalRequestBody renders a request body for the key: canonical JSON for
// a JSON body, a JSON string of its standard base64 for raw octets, and nil
// for no body at all.
func canonicalRequestBody(body []byte, binary bool) (*string, error) {
	if binary {
		text := canonicalJSONString(base64.StdEncoding.EncodeToString(body))
		return &text, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing request body data")
	}
	var rendered bytes.Buffer
	writeCanonicalJSON(&rendered, value)
	text := rendered.String()
	return &text, nil
}

// writeCanonicalJSON writes value with object keys sorted by code point, no
// insignificant whitespace, number lexemes unchanged and strings escaped the
// way JSON.stringify escapes them.
func writeCanonicalJSON(out *bytes.Buffer, value any) {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(typed))
	case json.Number:
		out.WriteString(typed.String())
	case string:
		out.WriteString(canonicalJSONString(typed))
	case []any:
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			writeCanonicalJSON(out, item)
		}
		out.WriteByte(']')
	case map[string]any:
		out.WriteByte('{')
		for i, name := range sortedKeys(typed) {
			if i > 0 {
				out.WriteByte(',')
			}
			out.WriteString(canonicalJSONString(name))
			out.WriteByte(':')
			writeCanonicalJSON(out, typed[name])
		}
		out.WriteByte('}')
	}
}

// canonicalJSONString quotes value exactly as JSON.stringify does: the quote,
// the backslash and the C0 controls are escaped, everything else is literal.
func canonicalJSONString(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				// Invalid UTF-8 ranges as U+FFFD, the character a TypeScript
				// runtime reads for the same bytes.
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

// cacheKeyEscape percent-encodes every byte outside A-Z a-z 0-9 - . _ ~, so a
// key is one unambiguous line whatever its values contain.
func cacheKeyEscape(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hexDigits[c>>4])
		out.WriteByte(hexDigits[c&0x0f])
	}
	return out.String()
}
