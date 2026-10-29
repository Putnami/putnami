package collaboration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
)

// ProvidedOperation is one operation a provider declares under its manifest's
// providers section.
type ProvidedOperation struct {
	// Provider is the extension's declaration of the contract: the version,
	// and the command, arguments, environment and timeout every operation of
	// the contract runs.
	Provider extension.ProviderDeclaration
	// Operation is the extension's declaration of this operation.
	Operation extension.ProviderOperation
	// Spec is the catalog's operation specification the declaration resolved to.
	Spec OperationSpec
}

// Preconditions returns how the provider enforces a revision precondition for
// the operation, empty when it declares none.
func (p ProvidedOperation) Preconditions() Preconditions {
	return Preconditions(p.Operation.Preconditions)
}

// ProviderOffer is everything one extension's manifest offers, keyed by
// contract, version and operation name. A contract version appears only when
// every one of its declarations is valid and every required operation is
// present.
type ProviderOffer map[string]map[int]map[string]ProvidedOperation

// Versions returns the versions of a contract the offer implements, ascending.
func (o ProviderOffer) Versions(contract string) []int {
	versions := make([]int, 0, len(o[contract]))
	for version := range o[contract] {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	return versions
}

// Operations returns one contract version's provided operations.
func (o ProviderOffer) Operations(contract string, version int) (map[string]ProvidedOperation, bool) {
	ops, ok := o[contract][version]
	return ops, ok
}

// ReadProviderOffer reads one extension's providers section and validates
// each declaration against the catalog. It is the one check the orchestrator
// applies when it resolves a binding and the one a provider author runs
// against their own manifest.
//
// A declaration naming a contract version this package does not implement is
// reported as a warning and ignored. Every other problem is an error that
// withdraws the contract from the offer: an unknown contract, a declaration
// without a version or a command, an unknown operation, a precondition rule
// the operation does not allow, an operation that takes the canonical
// selection without declaring workspaceSelection, or a missing required
// operation.
func ReadProviderOffer(providers map[string]extension.ProviderDeclaration) (ProviderOffer, []diag.Diagnostic) {
	contracts := make([]string, 0, len(providers))
	for contract := range providers {
		contracts = append(contracts, contract)
	}
	sort.Strings(contracts)

	offer := ProviderOffer{}
	var diags []diag.Diagnostic
	for _, contract := range contracts {
		declaration := providers[contract]
		field := "providers." + contract
		if !IsContract(contract) {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownContract, field, "unknown contract %q", contract))
			continue
		}
		if declaration.Version < 1 {
			diags = append(diags, diag.Errorf(ErrorCodeRequired, field+".version",
				"a provider declaration names its contract version"))
			continue
		}
		spec, known := Lookup(contract, declaration.Version)
		if !known {
			diags = append(diags, diag.Warningf(ErrorCodeUnsupportedVersion, field+".version",
				"%s version %d is not implemented by this orchestrator; the declaration is ignored",
				contract, declaration.Version))
			continue
		}
		operations, operationDiags := readProvidedOperations(field, contract, declaration, spec)
		diags = append(diags, operationDiags...)
		if diag.HasErrors(operationDiags) {
			continue
		}
		if offer[contract] == nil {
			offer[contract] = map[int]map[string]ProvidedOperation{}
		}
		offer[contract][declaration.Version] = operations
	}
	return offer, diags
}

// readProvidedOperations checks one contract's declaration against its
// catalog version and returns the operations it offers. Any error withdraws
// the whole contract version.
func readProvidedOperations(field, contract string, declaration extension.ProviderDeclaration, spec ContractSpec) (map[string]ProvidedOperation, []diag.Diagnostic) {
	c := &checker{}
	if strings.TrimSpace(declaration.Command) == "" {
		c.errorf(ErrorCodeRequired, field+".command", "a provider declaration names the command its operations run")
	}
	names := make([]string, 0, len(declaration.Operations))
	for name := range declaration.Operations {
		names = append(names, name)
	}
	sort.Strings(names)
	operations := map[string]ProvidedOperation{}
	for _, name := range names {
		operation := declaration.Operations[name]
		opField := field + ".operations." + name
		op, known := spec.Operation(name)
		if !known {
			c.errorf(ErrorCodeUnknownOperation, opField, "%s version %d has no operation %q", contract, declaration.Version, name)
			continue
		}
		before := len(c.diags)
		validateProvidedOperation(c, opField, contract, operation, op)
		if len(c.diags) > before {
			continue
		}
		operations[name] = ProvidedOperation{Provider: declaration, Operation: operation, Spec: op}
	}
	var missing []string
	for _, required := range spec.RequiredOperations() {
		if _, declared := declaration.Operations[required]; !declared {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		c.errorf(ErrorCodeInvalidDeclaration, field+".operations",
			"%s version %d is declared without its required operations: %s",
			contract, declaration.Version, strings.Join(missing, ", "))
	}
	return operations, c.diags
}

func validateProvidedOperation(c *checker, field, contract string, operation extension.ProviderOperation, op OperationSpec) {
	if op.Name == OperationCapabilities {
		c.errorf(ErrorCodeInvalidDeclaration, field, "capabilities is answered by the orchestrator")
		return
	}
	declared := Preconditions(operation.Preconditions)
	switch op.Preconditions {
	case PreconditionRuleNone:
		if declared != "" {
			c.errorf(ErrorCodeInvalidDeclaration, field+".preconditions",
				"%s.%s takes no revision precondition", contract, op.Name)
		}
	case PreconditionRuleDeclared:
		if !validPreconditions(declared) {
			c.errorf(ErrorCodeInvalidDeclaration, field+".preconditions",
				"%s.%s must declare preconditions: atomic, checked or none", contract, op.Name)
		}
	case PreconditionRuleAtomic:
		if declared != PreconditionsAtomic {
			c.errorf(ErrorCodeInvalidDeclaration, field+".preconditions",
				"%s.%s is offered only by a provider that enforces it atomically; declare preconditions: atomic",
				contract, op.Name)
		}
	}
	if op.WorkspaceSelection && !operation.WorkspaceSelection {
		c.errorf(ErrorCodeInvalidDeclaration, field+".workspaceSelection",
			"%s.%s takes the canonical selection arguments; the operation must declare workspaceSelection so the orchestrator resolves them",
			contract, op.Name)
	}
}

func validPreconditions(value Preconditions) bool {
	for _, known := range ValidPreconditions {
		if value == known {
			return true
		}
	}
	return false
}

// OperationKey names one operation of one contract version.
type OperationKey struct {
	// Contract is the contract name (tasks, proposals, memory, missions).
	Contract string
	// Version is the contract version.
	Version int
	// Operation is the operation name within Contract.
	Operation string
}

// String renders the key as "<contract>.<operation>@v<version>".
func (k OperationKey) String() string {
	return fmt.Sprintf("%s.%s@v%d", k.Contract, k.Operation, k.Version)
}

// Call is one validated operation request, as a provider handler receives it.
type Call struct {
	OperationKey
	// Input is the validated request document: a pointer to the operation's
	// input type (for example *TaskFindInput).
	Input any
	// Settings is the binding's settings object, verbatim.
	Settings json.RawMessage
	// Request is the whole provider request, for the workspace root and, when
	// the operation declared workspaceSelection, the resolved workspace view.
	Request ProviderRequest
}

// Failure is a non-ok outcome a handler returns.
type Failure struct {
	// Outcome is the closed non-ok result; see the Outcome* constants.
	Outcome Outcome
	// Error explains Outcome.
	Error Error
}

// Fail builds a Failure. Retryable defaults to false; an unavailable outcome
// the caller may retry sets it explicitly.
func Fail(outcome Outcome, reason, format string, args ...any) *Failure {
	return &Failure{Outcome: outcome, Error: Error{Message: fmt.Sprintf(format, args...), Reason: reason}}
}

// Handler performs one operation and returns its result document, or a
// Failure.
type Handler func(ctx context.Context, call Call) (any, *Failure)

// Serve answers one routed provider call: it reads one ProviderRequest from
// in, validates the request against the operation the orchestrator named,
// runs the handler, validates the result, and writes exactly one Response
// document. A request that did not come through a binding (no provider
// member) is refused as invalid.
//
// A handler that panics or returns a result the contract refuses answers
// unresolved for a mutation — the write may have happened — and unavailable
// for a read. A list page too large for the document bound is requested again
// with fewer items (see fitResult).
func Serve(ctx context.Context, in io.Reader, out io.Writer, handlers map[OperationKey]Handler) error {
	var request ProviderRequest
	decoder := json.NewDecoder(in)
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode collaboration provider request: %w", err)
	}
	return writeResponse(out, serveOne(ctx, request, handlers))
}

func serveOne(ctx context.Context, request ProviderRequest, handlers map[OperationKey]Handler) Response {
	if request.Provider == nil {
		return failureResponse(Fail(OutcomeInvalid, ReasonRequestInvalid,
			"a collaboration provider is called only through a workspace binding; the request names no operation"))
	}
	key := OperationKey{Contract: request.Provider.Contract, Version: request.Provider.Version, Operation: request.Provider.Operation}
	handler, ok := handlers[key]
	if !ok {
		return failureResponse(Fail(OutcomeUnsupported, ReasonOperationUnsupported,
			"this provider does not implement %s", key))
	}
	spec, _ := Lookup(key.Contract, key.Version)
	op, _ := spec.Operation(key.Operation)
	input, diags := ParseRequest(key.Contract, key.Version, key.Operation, request.Arguments)
	if diags != nil {
		return failureResponse(Fail(OutcomeInvalid, ReasonRequestInvalid, "%s", formatDiagnostics(diags)))
	}
	call := Call{
		OperationKey: key,
		Input:        input,
		Settings:     request.Provider.Settings,
		Request:      request,
	}
	result, failure := runHandler(ctx, handler, call, op)
	if failure != nil {
		return failureResponse(failure)
	}
	encoded, failure := fitResult(ctx, handler, call, op, result)
	if failure != nil {
		return failureResponse(failure)
	}
	if _, diags := ParseResult(key.Contract, key.Version, key.Operation, encoded); diags != nil {
		return failureResponse(uncertain(op, "the %s result violates the contract: %s", key, formatDiagnostics(diags)))
	}
	return Response{Outcome: OutcomeOK, Result: encoded}
}

func runHandler(ctx context.Context, handler Handler, call Call, op OperationSpec) (result any, failure *Failure) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			failure = uncertain(op, "the %s handler panicked: %v", call.OperationKey, recovered)
		}
	}()
	result, failure = handler(ctx, call)
	if failure != nil && failure.Outcome == OutcomeOK {
		return nil, uncertain(op, "the %s handler returned a failure with outcome ok", call.OperationKey)
	}
	return result, failure
}

// maxResultBytes bounds the result document Serve answers with. The rest of
// MaxDocumentBytes holds the Response around it and the members the
// orchestrator adds to make it an Envelope.
const maxResultBytes = MaxDocumentBytes - 4<<10

// listOverheadBytes bounds a list result without its items: the members
// around them and a page.next cursor of MaxCursorLength characters, each
// escaped to at most six bytes.
const listOverheadBytes = 64 + 6*MaxCursorLength

// fitResult encodes a handler's result. A list page that would encode above
// maxResultBytes is requested again with a smaller page size, the number of
// its first items that fit, so the provider issues the cursor that follows
// the shorter page and a traversal still covers every item once. The page
// size strictly shrinks on every repeat, and only a read returns a list, so
// repeating the handler has no effect a caller could see.
func fitResult(ctx context.Context, handler Handler, call Call, op OperationSpec, result any) ([]byte, *Failure) {
	for asked := 0; ; {
		emptyListsAsEmpty(result)
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, uncertain(op, "encode the %s result: %v", call.OperationKey, err)
		}
		if len(encoded) <= maxResultBytes {
			return encoded, nil
		}
		sizes, err := listItemSizes(result)
		if err != nil {
			return nil, uncertain(op, "encode the %s result: %v", call.OperationKey, err)
		}
		page := pageRequestOf(call.Input)
		if sizes == nil || page == nil {
			return encoded, nil
		}
		if asked > 0 && len(sizes) > asked {
			return nil, uncertain(op, "the %s handler returned %d items for a page of %d", call.OperationKey, len(sizes), asked)
		}
		fit, total := 0, listOverheadBytes
		for _, size := range sizes {
			if total += size + 1; total > maxResultBytes {
				break
			}
			fit++
		}
		if fit == 0 {
			return nil, uncertain(op, "one %s item encodes to %d bytes, above the %d-byte result bound", call.OperationKey, sizes[0], maxResultBytes)
		}
		asked = fit
		page.Size = fit
		var failure *Failure
		if result, failure = runHandler(ctx, handler, call, op); failure != nil {
			return nil, failure
		}
	}
}

// listItemSizes returns the encoded size of every item of a list result, or
// nil for any other result.
func listItemSizes(result any) ([]int, error) {
	switch list := result.(type) {
	case *TaskListResult:
		return encodedSizes(list.Items)
	case *ProposalListResult:
		return encodedSizes(list.Items)
	case *ProposalCommentListResult:
		return encodedSizes(list.Items)
	case *MemoryListResult:
		return encodedSizes(list.Items)
	}
	return nil, nil
}

func encodedSizes[T any](items []T) ([]int, error) {
	sizes := make([]int, len(items))
	for i, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		sizes[i] = len(encoded)
	}
	return sizes, nil
}

// pageRequestOf returns the page request of a list operation's input,
// creating it when the request carried none, or nil for any other input.
func pageRequestOf(input any) *PageRequest {
	var page **PageRequest
	switch in := input.(type) {
	case *TaskFindInput:
		page = &in.Page
	case *ProposalFindInput:
		page = &in.Page
	case *ProposalCommentsInput:
		page = &in.Page
	case *MemoryContextInput:
		page = &in.Page
	case *MemorySearchInput:
		page = &in.Page
	default:
		return nil
	}
	if *page == nil {
		*page = &PageRequest{}
	}
	return *page
}

// emptyListsAsEmpty turns a result's nil lists into the empty lists the
// contract requires, so a handler that found nothing may return a nil slice.
func emptyListsAsEmpty(result any) {
	switch list := result.(type) {
	case *MissionsSyncResult:
		if list.Mission.Plan.Steps == nil {
			list.Mission.Plan.Steps = []PlanStep{}
		}
		if list.Mission.OpenDecisions == nil {
			list.Mission.OpenDecisions = []OpenDecision{}
		}
		if list.Inbox == nil {
			list.Inbox = []InboxMessage{}
		}
		if list.Decisions == nil {
			list.Decisions = []DecisionStatus{}
		}
		if list.Replies == nil {
			list.Replies = []ReplyStatus{}
		}
	case *TaskListResult:
		if list.Items == nil {
			list.Items = []Task{}
		}
	case *ProposalListResult:
		if list.Items == nil {
			list.Items = []Proposal{}
		}
	case *ProposalCommentListResult:
		if list.Items == nil {
			list.Items = []ProposalComment{}
		}
	case *MemoryListResult:
		if list.Items == nil {
			list.Items = []MemoryRecord{}
		}
	}
}

// uncertain is the failure of a handler whose effect is unknown: unresolved
// for a mutation, unavailable for a read.
func uncertain(op OperationSpec, format string, args ...any) *Failure {
	if op.Access == AccessMutating {
		return Fail(OutcomeUnresolved, ReasonProviderInvalidResponse, format, args...)
	}
	return Fail(OutcomeUnavailable, ReasonProviderInvalidResponse, format, args...)
}

func failureResponse(failure *Failure) Response {
	errorValue := failure.Error
	if failure.Outcome == OutcomeUnresolved {
		errorValue.Retryable = false
	}
	return Response{Outcome: failure.Outcome, Error: &errorValue}
}

// writeResponse writes the one Response document a provider answers with,
// followed by a newline, in a single write.
func writeResponse(out io.Writer, response Response) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return fmt.Errorf("encode collaboration response: %w", err)
	}
	_, err := out.Write(buffer.Bytes())
	return err
}

// formatDiagnostics renders error diagnostics on one line.
func formatDiagnostics(diags []diag.Diagnostic) string {
	parts := make([]string, 0, len(diags))
	for _, d := range diag.Errors(diags) {
		if d.Field != "" {
			parts = append(parts, d.Field+": "+d.Message)
		} else {
			parts = append(parts, d.Message)
		}
	}
	return strings.Join(parts, "; ")
}

// FormatDiagnostics renders error diagnostics on one line, for messages.
func FormatDiagnostics(diags []diag.Diagnostic) string { return formatDiagnostics(diags) }
