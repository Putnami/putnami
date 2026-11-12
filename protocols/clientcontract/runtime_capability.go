package clientcontract

import "slices"

// GeneratedLanguage identifies a supported generated client language.
type GeneratedLanguage string

// Supported generated client languages.
const (
	GeneratedLanguageGo         GeneratedLanguage = "go"
	GeneratedLanguageTypeScript GeneratedLanguage = "ts"
)

// RuntimeCapability names one behavior a generated client needs from the
// client runtime beyond the base protocol. The vocabulary is closed. A
// capability this package names but the runtime of a client's language does not
// implement (RuntimeCapabilitiesImplementedBy) is one that runtime does not
// honor yet, so a generated client that requires it fails to load rather than
// run without the declared behavior.
type RuntimeCapability string

// Runtime capabilities a generated client can require.
const (
	// RuntimeCapabilityResponseCache is the per-operation response cache:
	// fresh answers from memory, singleflight on a miss, serve-stale on a
	// provider failure, and invalidation by key prefix.
	RuntimeCapabilityResponseCache RuntimeCapability = "response-cache"
	// RuntimeCapabilitySSEContinuation is the negotiated SSE wire and its
	// continuation (ADR 0013): the wire marker and explicit terminal, the
	// position of the last delivered message, and one session across the
	// reopened connections.
	RuntimeCapabilitySSEContinuation RuntimeCapability = "sse-continuation"
)

// ImplementedRuntimeCapabilities is the closed set of capabilities that both
// the Go and the TypeScript client runtimes released with this protocol version
// implement.
//
// One runtime may implement a capability before the other: a generated client
// pins its requirement against the runtime of its own language
// (RuntimeCapabilitiesImplementedBy), so a Go client may require a capability
// the TypeScript runtime does not have yet. The set grows when the last
// runtime catches up.
var ImplementedRuntimeCapabilities = commonRuntimeCapabilities()

// runtimeCapabilitiesByLanguage is what each client runtime implements, in
// ascending order. Each runtime pins its own list to its row in its tests.
//
// Both runtimes implement SSE continuation (ADR 0013); the rows stay separate
// so the next capability can land in one runtime before the other.
var runtimeCapabilitiesByLanguage = map[GeneratedLanguage][]RuntimeCapability{
	GeneratedLanguageGo:         {RuntimeCapabilityResponseCache, RuntimeCapabilitySSEContinuation},
	GeneratedLanguageTypeScript: {RuntimeCapabilityResponseCache, RuntimeCapabilitySSEContinuation},
}

// RuntimeCapabilitiesImplementedBy returns, in ascending order, the capabilities
// the client runtime of language implements. An unknown language implements
// none.
func RuntimeCapabilitiesImplementedBy(language GeneratedLanguage) []RuntimeCapability {
	return append([]RuntimeCapability(nil), runtimeCapabilitiesByLanguage[language]...)
}

// commonRuntimeCapabilities is the ascending intersection of every runtime's
// capabilities.
func commonRuntimeCapabilities() []RuntimeCapability {
	var common []RuntimeCapability
	for _, capability := range runtimeCapabilitiesByLanguage[GeneratedLanguageGo] {
		shared := true
		for _, capabilities := range runtimeCapabilitiesByLanguage {
			shared = shared && slices.Contains(capabilities, capability)
		}
		if shared {
			common = append(common, capability)
		}
	}
	return common
}

// RequiredRuntimeCapabilities derives the capabilities a set of generated
// operations requires, in ascending order: the response cache when an
// operation declares a cache policy, and SSE continuation when one of its
// transports declares a continuation.
func RequiredRuntimeCapabilities(operations []GeneratedOperation) []RuntimeCapability {
	cache, continuation := false, false
	for _, operation := range operations {
		cache = cache || operation.Cache != nil
		for _, transport := range operation.Transports {
			continuation = continuation || transport.Continuation() != nil
		}
	}
	var required []RuntimeCapability
	if cache {
		required = append(required, RuntimeCapabilityResponseCache)
	}
	if continuation {
		required = append(required, RuntimeCapabilitySSEContinuation)
	}
	return required
}

// GeneratedOperation is one contract operation as a generated client carries
// it: its stream mode, its transports and its response cache policy, which
// decide the runtime capabilities the client requires.
type GeneratedOperation struct {
	// OperationID is the provider-authored stable operation identity.
	OperationID string `json:"operationId"`
	// Stream records the operation's message cardinality.
	Stream StreamMode `json:"stream"`
	// Transports preserves the provider's advertised transport order.
	Transports []Transport `json:"transports"`
	// Cache is the operation's declared response cache policy.
	Cache *CachePolicy `json:"cache,omitempty"`
}
