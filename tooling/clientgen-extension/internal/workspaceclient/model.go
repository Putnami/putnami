// Package workspaceclient discovers and verifies first-party generated clients.
package workspaceclient

import (
	"time"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// Mode names what produced a report. The guard is the only producer left.
type Mode string

// ModeCheck inspects the committed inputs without trusting build rewrites.
const ModeCheck Mode = "check"

// Classification distinguishes strict Putnami contracts from external inputs.
type Classification string

const (
	// ClassificationFirstParty marks an x-putnami-client v1 provider contract.
	ClassificationFirstParty Classification = "first-party"
	// ClassificationThirdParty marks an unmarked external OpenAPI input.
	ClassificationThirdParty Classification = "third-party"
)

// Finding is one actionable workspace client adoption or drift failure.
type Finding struct {
	Code      string `json:"code"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	Column    int    `json:"column,omitempty"`
	ServiceID string `json:"serviceId,omitempty"`
	Message   string `json:"message"`
}

// TransportCallsite is one concrete source expression that opens or executes
// a low-level transport. Line and column are one-based and point at the called
// transport identifier.
type TransportCallsite struct {
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Column      int    `json:"column"`
	Transport   string `json:"transport"`
	Symbol      string `json:"symbol"`
	Fingerprint string `json:"fingerprint"`
}

// ExternalCallsite is the durable identity authorized by the external
// inventory. Source coordinates are omitted deliberately: inserting a line
// above an unchanged expression must not invalidate its authority.
type ExternalCallsite struct {
	Path        string `json:"path"`
	Transport   string `json:"transport"`
	Symbol      string `json:"symbol"`
	Fingerprint string `json:"fingerprint"`
}

// Adaptation records a source-aware client migration candidate or applied edit.
type Adaptation struct {
	Path        string              `json:"path"`
	Language    string              `json:"language"`
	ServiceID   string              `json:"serviceId"`
	Callsite    *TransportCallsite  `json:"callsite,omitempty"`
	Evidence    []string            `json:"evidence"`
	Bindings    []AdaptationBinding `json:"bindings"`
	Disposition string              `json:"disposition"`
	Reason      string              `json:"reason"`
}

// AdaptationBinding is a generated registration a consumer can call instead of
// a handwritten transport. Every symbol derives from a ClientReport.
type AdaptationBinding struct {
	ProviderProject string `json:"providerProject"`
	ImportPath      string `json:"importPath"`
	Service         string `json:"service"`
	ClientSymbol    string `json:"clientSymbol"`
	BindingSymbol   string `json:"bindingSymbol"`
}

// ClientReport is one generated client of a provider: a client project that
// names the provider's service in generatedFrom, or the in-module Go client
// the provider's committed clientgen.json declares. Its symbols derive from
// the committed contract and the generation options, never from generated
// bytes, which Git does not hold.
type ClientReport struct {
	// Project is the project that generates the client: the client project,
	// or the provider for an in-module client.
	Project    string                           `json:"project"`
	Language   clientcontract.GeneratedLanguage `json:"language"`
	ImportPath string                           `json:"importPath"`
	// Bindings lists the generated service registrations in canonical order.
	Bindings []ClientBinding `json:"bindings"`
}

// ClientBinding is one generated service registration of a client.
type ClientBinding struct {
	// Service is the generated service the registration binds.
	Service string `json:"service"`
	// ClientSymbol is the generated public client type name.
	ClientSymbol string `json:"clientSymbol"`
	// BindingSymbol is the generated typed registration function.
	BindingSymbol string `json:"bindingSymbol"`
}

// ExternalContract records explicit authority for an unmarked third-party input.
type ExternalContract struct {
	Project   string             `json:"project"`
	Authority string             `json:"authority"`
	Adapter   string             `json:"adapter"`
	Callsites []ExternalCallsite `json:"callsites"`
	Owner     string             `json:"owner"`
	Tests     []string           `json:"tests"`
	Reason    string             `json:"reason"`
}

// FrameworkTransport records a low-level callsite that no generated binding can
// replace. Runtime is validated against the exact indexed project identity, so
// a consumer cannot claim framework ownership for its own calls.
type FrameworkTransport struct {
	Project   string             `json:"project"`
	Runtime   string             `json:"runtime"`
	Status    string             `json:"status"`
	Adapter   string             `json:"adapter"`
	Callsites []ExternalCallsite `json:"callsites"`
	Owner     string             `json:"owner"`
	Tests     []string           `json:"tests"`
	Reason    string             `json:"reason"`
	// Operations names the provider operations a StatusPendingProviderContract
	// entry is waiting on, and PendingWork names the work that declares them.
	// Both are required for that status and forbidden for the others: a
	// pending state has to say what it is pending on, and a settled one must
	// not pretend to be temporary.
	Operations  []string `json:"operations,omitempty"`
	PendingWork string   `json:"pendingWork,omitempty"`
}

const (
	// StatusFrameworkRuntime marks the transport that a generated binding runs
	// on. Only @putnami/client and go.putnami.dev/client can hold it: replacing
	// one of these callsites with a generated client would be circular.
	StatusFrameworkRuntime = "framework-runtime"
	// StatusTransportPrimitive marks a generic transport whose endpoint and
	// contract are supplied by the caller. No service contract crosses the
	// callsite, so there is no declaration to generate a client from.
	StatusTransportPrimitive = "transport-primitive"
	// StatusPendingProviderContract marks a first-party Putnami service whose
	// provider-side client declaration does not exist yet. It is an explicit,
	// versioned state with named operations and named closing work, not an
	// exemption: the entry fails the moment its callsites move or disappear.
	StatusPendingProviderContract = "pending-provider-contract"
)

// ProviderReport reports a provider's classification and its generated clients.
type ProviderReport struct {
	Project        string         `json:"project"`
	Classification Classification `json:"classification"`
	ServiceID      string         `json:"serviceId,omitempty"`
	Audience       string         `json:"audience,omitempty"`
	ContractSHA256 string         `json:"contractSha256,omitempty"`
	Clients        []ClientReport `json:"clients,omitempty"`
	// EmptyContract marks a first-party provider whose contract declares a
	// service identity and no first-party operation: every route is owned by an
	// external authority, or none is served. It is the report's half of the
	// declaration the committed contract sidecar carries, and the reason such a
	// provider needs no client (ADR 0004).
	//
	// A consumer that reaches such a service has no generated binding to switch
	// to and never will, so bindingAdvice says to classify the callsite instead
	// of naming a client that cannot exist.
	EmptyContract bool `json:"emptyContract,omitempty"`
}

// Report is the deterministic workspace client verification result.
type Report struct {
	ProtocolVersion     int                  `json:"protocolVersion"`
	Mode                Mode                 `json:"mode"`
	Providers           []ProviderReport     `json:"providers"`
	ExternalContracts   []ExternalContract   `json:"externalContracts"`
	FrameworkTransports []FrameworkTransport `json:"frameworkTransports"`
	AdaptationQueue     []Adaptation         `json:"adaptationQueue"`
	Findings            []Finding            `json:"findings"`
	// PendingTransports are handwritten-transport verdicts the workspace's
	// committed census already accounts for. They are reported and counted but
	// do not fail the run, which is what lets the guard land before the
	// migration that empties it finishes (see pending.go). A finding that is
	// NOT censused stays in Findings and fails.
	PendingTransports []Finding `json:"pendingTransports"`
	// Timings is the wall time each phase of the run spent, in milliseconds,
	// keyed by PhaseName. The session record exposes the task as one
	// durationMs; this is the split behind it, so a slow guard is diagnosed
	// from the report rather than estimated.
	Timings map[string]int64 `json:"timings,omitempty"`
}

// Phase names for Report.Timings and the matching metric events. The same
// constant is the metric name, prefixed by PhaseMetricPrefix, so a reader of
// the session's event log and a reader of the report see one vocabulary.
const (
	// PhaseInspect discovers providers and their clients and validates the
	// inventories.
	PhaseInspect = "inspect"
	// PhaseScan parses every consumer source for bindings and transports.
	PhaseScan = "scan"
	// PhaseMetricPrefix prefixes each phase name in the emitted metric.
	PhaseMetricPrefix = "clientgen.phase."
)

// recordTiming stores one phase duration on the report, creating the map on
// first use.
func (r *Report) recordTiming(phase string, elapsed time.Duration) {
	if r.Timings == nil {
		r.Timings = map[string]int64{}
	}
	r.Timings[phase] = elapsed.Milliseconds()
}

// Clean reports whether the run found no finding. Censused pending transports are deliberately excluded: they are reported
// debt, not a failure of the change under test.
func (r Report) Clean() bool { return len(r.Findings) == 0 }
