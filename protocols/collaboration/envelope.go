package collaboration

import "encoding/json"

// Outcome is the closed result vocabulary of every operation. Only OutcomeOK
// carries a result; every other outcome carries an Error.
type Outcome string

// Outcomes.
const (
	// OutcomeOK: the operation completed and Result holds its document.
	OutcomeOK Outcome = "ok"
	// OutcomeNotFound: a referenced item does not exist.
	OutcomeNotFound Outcome = "not_found"
	// OutcomeConflict: a precondition failed — a stale expectedRevision, an
	// existing record under mustNotExist, a claim held by someone else, an
	// idempotency key reused with different content. Error.Current carries the
	// current revision when the provider knows it. Nothing was written.
	OutcomeConflict Outcome = "conflict"
	// OutcomeUnsupported: the contract is unbound, or the bound provider does
	// not offer the operation, the version, or the requested precondition.
	// Nothing was attempted.
	OutcomeUnsupported Outcome = "unsupported"
	// OutcomeInvalid: the request violates the contract. Nothing was attempted.
	OutcomeInvalid Outcome = "invalid"
	// OutcomeDenied: the provider refused the caller's credentials or
	// permissions. Nothing was written.
	OutcomeDenied Outcome = "denied"
	// OutcomeUnavailable: the provider could not be reached or could not
	// answer, and no write can have happened: every read failure, and a
	// mutation that failed before its provider process started.
	OutcomeUnavailable Outcome = "unavailable"
	// OutcomeUnresolved: a mutation may or may not have happened. The caller
	// reconciles with a read (Error.Reconcile says which) before any retry, and
	// automatic retries stop. Never retryable.
	OutcomeUnresolved Outcome = "unresolved"
)

// ValidOutcomes is the closed outcome vocabulary in canonical order.
var ValidOutcomes = []Outcome{
	OutcomeConflict, OutcomeDenied, OutcomeInvalid, OutcomeNotFound,
	OutcomeOK, OutcomeUnavailable, OutcomeUnresolved, OutcomeUnsupported,
}

// Error explains a non-ok outcome.
type Error struct {
	// Message is for a person; automation branches on Reason.
	Message string `json:"message"`
	// Reason is a stable machine-readable code: one of the Reason* constants
	// when the orchestrator produced the outcome, a provider-chosen dotted code
	// otherwise.
	Reason string `json:"reason,omitempty"`
	// Retryable states whether repeating the identical request can succeed
	// without anything else changing. An unresolved outcome is never retryable.
	Retryable bool `json:"retryable"`
	// Current is the item's current revision, on a conflict.
	Current string `json:"current,omitempty"`
	// Reconcile names the read, or the idempotent repeat, that establishes
	// whether an unresolved mutation happened. It is prose for a person that
	// names that operation as "<contract>.<operation>" of the same contract;
	// ReconcileOperation returns it for automation.
	Reconcile string `json:"reconcile,omitempty"`
}

// Response is the document a provider writes as the single text block of its
// ToolCallResult. The orchestrator validates it and wraps it into an Envelope.
type Response struct {
	// Outcome is the closed result of the operation; see the Outcome* constants.
	Outcome Outcome `json:"outcome"`
	// Result is the operation-specific result document, present only on
	// OutcomeOK.
	Result json.RawMessage `json:"result,omitempty"`
	// Error explains a non-ok outcome; absent on OutcomeOK.
	Error *Error `json:"error,omitempty"`
}

// ProviderIdentity names the provider extension that answered.
type ProviderIdentity struct {
	// Name is the provider extension's identifier.
	Name string `json:"name"`
	// Version is the provider extension's resolved version, when known.
	Version string `json:"version,omitempty"`
}

// Envelope is the answer to every operation call on every entry path: the MCP
// tool result text and the CLI's standard output carry the same document.
// Contract, Version, Operation and Provider are the orchestrator's statement of
// what was routed where; a provider cannot set them.
type Envelope struct {
	// Contract is the operation contract's name (tasks, proposals, memory).
	Contract string `json:"contract"`
	// Version is the contract version the orchestrator routed against.
	Version int `json:"version,omitempty"`
	// Operation is the operation name within Contract.
	Operation string `json:"operation"`
	// Provider identifies the extension that answered, when a provider is
	// bound.
	Provider *ProviderIdentity `json:"provider,omitempty"`
	// Outcome is the closed result of the operation; see the Outcome* constants.
	Outcome Outcome `json:"outcome"`
	// Result is the operation-specific result document, present only on
	// OutcomeOK.
	Result json.RawMessage `json:"result,omitempty"`
	// Error explains a non-ok outcome; absent on OutcomeOK.
	Error *Error `json:"error,omitempty"`
}

// Reasons the orchestrator reports. A provider may use the provider-owned
// reasons (ReasonIdempotencyMismatch, ReasonPrecondition...) and its own
// dotted codes.
const (
	ReasonBindingMissing          = "binding.missing"
	ReasonBindingInvalid          = "binding.invalid"
	ReasonBindingAmbiguous        = "binding.ambiguous"
	ReasonBindingProviderMissing  = "binding.provider_missing"
	ReasonBindingVersion          = "binding.version_unsupported"
	ReasonBindingIncomplete       = "binding.incomplete"
	ReasonOperationUnsupported    = "operation.unsupported"
	ReasonPreconditionUnsupported = "precondition.unsupported"
	ReasonRequestInvalid          = "request.invalid"
	ReasonProviderUnavailable     = "provider.unavailable"
	ReasonProviderTimeout         = "provider.timeout"
	ReasonProviderCanceled        = "provider.canceled"
	ReasonProviderFailed          = "provider.failed"
	ReasonProviderInvalidResponse = "provider.invalid_response"
	ReasonProviderIdentity        = "provider.identity_mismatch"
	ReasonIdempotencyMismatch     = "idempotency.mismatch"
	ReasonRevisionConflict        = "revision.conflict"
	ReasonAlreadyExists           = "record.exists"
	ReasonClaimHeld               = "claim.held"
)

// BindingStatus is the state of one contract's binding in a workspace.
type BindingStatus string

// Binding statuses.
const (
	// BindingStatusBound: a valid binding names an available provider that
	// implements every required operation of the bound version.
	BindingStatusBound BindingStatus = "bound"
	// BindingStatusUnbound: the workspace binds no provider for the contract.
	BindingStatusUnbound BindingStatus = "unbound"
	// BindingStatusInvalid: the workspace binds the contract, and the binding
	// cannot be honored. Diagnostics say why.
	BindingStatusInvalid BindingStatus = "invalid"
)

// ValidBindingStatuses is the closed binding-status vocabulary in canonical order.
var ValidBindingStatuses = []BindingStatus{BindingStatusBound, BindingStatusInvalid, BindingStatusUnbound}

// Capabilities is the capabilities result: what the workspace bound for one
// contract, and what that provider does and does not offer. The orchestrator
// produces it from the binding and the provider's manifest, without running
// the provider.
type Capabilities struct {
	// Contract is the contract this report describes (tasks, proposals, memory).
	Contract string `json:"contract"`
	// SupportedVersions are the contract versions this orchestrator speaks.
	SupportedVersions []int `json:"supportedVersions"`
	// Status is whether the workspace's binding for Contract is usable; see
	// the BindingStatus* constants.
	Status BindingStatus `json:"status"`
	// Binding is the workspace's raw binding for Contract, when one exists.
	Binding *Binding `json:"binding,omitempty"`
	// Provider identifies the bound provider extension, when Status is bound.
	Provider *ProviderIdentity `json:"provider,omitempty"`
	// Operations lists every operation of the bound version, supported or
	// not, in canonical order. It is empty when no version is bound.
	Operations []OperationCapability `json:"operations"`
	// Issues say why a binding is invalid.
	Issues []CapabilityIssue `json:"issues,omitempty"`
	// Candidates are the available extensions that declare this contract. They
	// are reported, never selected: only the binding chooses a provider.
	Candidates []ProviderIdentity `json:"candidates,omitempty"`
}

// OperationCapability is one operation's support by the bound provider.
type OperationCapability struct {
	// Name is the operation's name within its contract.
	Name string `json:"name"`
	// Access states whether the operation reads or mutates provider state.
	Access Access `json:"access"`
	// Required states whether the contract mandates this operation.
	Required bool `json:"required"`
	// Supported states whether the bound provider offers this operation.
	Supported bool `json:"supported"`
	// Preconditions is how the provider enforces expectedRevision, for an
	// operation that takes one.
	Preconditions Preconditions `json:"preconditions,omitempty"`
	// OpenWorld is the provider's annotation: whether the operation reaches a
	// system outside this machine.
	OpenWorld *bool `json:"openWorld,omitempty"`
	// Description is the provider tool's own description, which states
	// provider-specific behavior and limits.
	Description string `json:"description,omitempty"`
}

// CapabilityIssue is one reason a binding cannot be honored.
type CapabilityIssue struct {
	// Reason is a stable machine-readable code for why the binding is invalid.
	Reason string `json:"reason"`
	// Message explains Reason for a person.
	Message string `json:"message"`
}
