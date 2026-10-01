// Package workspaceclient discovers and verifies first-party generated clients.
package workspaceclient

import (
	"time"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// Mode selects synchronization or committed-byte verification.
type Mode string

const (
	// ModeSync regenerates configured targets before inspecting worktree bytes.
	ModeSync Mode = "sync"
	// ModeAdopt regenerates targets and applies only manifest-proven import moves.
	ModeAdopt Mode = "adopt"
	// ModeCheck inspects committed client bytes without trusting build rewrites.
	ModeCheck Mode = "check"
)

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

// AdaptationBinding is an actual generated registration candidate. Every
// symbol comes from a validated emitter manifest rather than a naming guess.
type AdaptationBinding struct {
	ProviderProject string `json:"providerProject"`
	ImportPath      string `json:"importPath"`
	Service         string `json:"service"`
	ClientSymbol    string `json:"clientSymbol"`
	BindingSymbol   string `json:"bindingSymbol"`
}

// Coverage reports operation-target coverage from generated descriptors.
type Coverage struct {
	Required int `json:"required"`
	Covered  int `json:"covered"`
	Percent  int `json:"percent"`
}

// TargetReport reports one configured generated target.
type TargetReport struct {
	Language            clientcontract.GeneratedLanguage    `json:"language"`
	Output              string                              `json:"output"`
	Manifest            string                              `json:"manifest"`
	Binding             *clientcontract.GeneratedBinding    `json:"binding,omitempty"`
	GeneratedOperations []clientcontract.GeneratedOperation `json:"generatedOperations,omitempty"`
	Operations          int                                 `json:"operations"`
	Covered             int                                 `json:"covered"`
	// Omitted names the provider operations the target's configuration leaves
	// out, as its manifest records them. They count in Operations and never in
	// Covered.
	Omitted []string `json:"omitted,omitempty"`
}

// ConsumerEdge joins a provider operation to an imported typed binding.
type ConsumerEdge struct {
	ProviderProject   string                           `json:"providerProject"`
	ServiceID         string                           `json:"serviceId"`
	OperationID       string                           `json:"operationId"`
	Stream            clientcontract.StreamMode        `json:"stream"`
	Language          clientcontract.GeneratedLanguage `json:"language"`
	GeneratedArtifact string                           `json:"generatedArtifact"`
	ConsumerProject   string                           `json:"consumerProject"`
	BindingImport     string                           `json:"bindingImport"`
	ClientSymbol      string                           `json:"clientSymbol"`
	BindingSymbol     string                           `json:"bindingSymbol"`
	MethodSymbol      string                           `json:"methodSymbol"`
	AuthProfiles      []string                         `json:"authProfiles"`
	Transports        []clientcontract.Transport       `json:"transports"`
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

// ProviderReport reports provider classification and generated target coverage.
type ProviderReport struct {
	Project        string         `json:"project"`
	Classification Classification `json:"classification"`
	ServiceID      string         `json:"serviceId,omitempty"`
	Audience       string         `json:"audience,omitempty"`
	ContractSHA256 string         `json:"contractSha256,omitempty"`
	Targets        []TargetReport `json:"targets,omitempty"`
	// EmptyContract marks a first-party provider whose contract declares a
	// service identity and no first-party operation: every route is owned by an
	// external authority, or none is served. It is the report's half of the
	// declaration the committed contract sidecar carries, and the reason such a
	// provider names no target — Targets is empty for one, because a target
	// with nothing to generate produced nothing to report (ADR 0004).
	//
	// A consumer that reaches such a service has no generated binding to switch
	// to and never will, so bindingAdvice says to classify the callsite instead
	// of telling the developer to regenerate a client that cannot exist.
	EmptyContract bool `json:"emptyContract,omitempty"`
}

// Report is the deterministic workspace client synchronization result.
type Report struct {
	ProtocolVersion     int                  `json:"protocolVersion"`
	Mode                Mode                 `json:"mode"`
	Coverage            Coverage             `json:"coverage"`
	Providers           []ProviderReport     `json:"providers"`
	ConsumerEdges       []ConsumerEdge       `json:"consumerEdges"`
	ExternalContracts   []ExternalContract   `json:"externalContracts"`
	FrameworkTransports []FrameworkTransport `json:"frameworkTransports"`
	AppliedAdaptations  []Adaptation         `json:"appliedAdaptations"`
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
	// PhaseSnapshot captures the generated manifests before an adoption
	// regenerates them (adopt only).
	PhaseSnapshot = "snapshot"
	// PhaseMaterialize builds every provider through the spawning CLI (sync and
	// adopt only).
	PhaseMaterialize = "materialize"
	// PhaseSynchronize regenerates targets in place (sync and adopt only).
	PhaseSynchronize = "synchronize"
	// PhaseRender renders the expected clients into a temporary mirror (sync
	// and adopt only).
	PhaseRender = "render"
	// PhaseInspect compares manifests, generated files and inventories.
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

// Clean reports whether every first-party generated target is current.
// Censused pending transports are deliberately excluded: they are reported
// debt, not a failure of the change under test.
func (r Report) Clean() bool { return len(r.Findings) == 0 }

// WithFindings merges findings observed outside inspection — the pre-build
// snapshot is the only current source — and restores the report's canonical
// order so two runs of the same workspace produce the same bytes.
func (r Report) WithFindings(extra ...Finding) Report {
	if len(extra) == 0 {
		return r
	}
	r.Findings = append(append([]Finding(nil), r.Findings...), extra...)
	canonicalizeReport(&r)
	return r
}
